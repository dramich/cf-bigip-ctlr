package router

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	VcapBackendHeader = "X-Vcap-Backend"
	VcapRouterHeader  = "X-Vcap-Router"
	VcapTraceHeader   = "X-Vcap-Trace"

	VcapCookieId    = "__VCAP_ID__"
	StickyCookieKey = "JSESSIONID"
)

type Proxy struct {
	sync.RWMutex
	*Registry

	r          map[string][]*registerMessage
	d          map[string]int
	varz       *Varz
	se         *SessionEncoder
	activeApps *AppList
}

func NewProxy(se *SessionEncoder, activeApps *AppList, varz *Varz, r *Registry) *Proxy {
	p := new(Proxy)

	p.Registry = r
	p.r = make(map[string][]*registerMessage)
	p.d = make(map[string]int)

	p.se = se
	p.varz = varz
	p.activeApps = activeApps

	return p
}

func (p *Proxy) Lookup(req *http.Request) *registerMessage {
	p.RLock()
	s := p.Registry.Lookup(req)
	p.RUnlock()

	if s == nil {
		return nil
	}

	// If there's only one backend, choose that
	if len(s) == 1 {
		return s[0]
	}

	// Choose backend depending on sticky session
	var sticky string
	for _, v := range req.Cookies() {
		if v.Name == VcapCookieId {
			sticky = v.Value
			break
		}
	}

	var rm *registerMessage
	if sticky != "" {
		sHost, sPort := p.se.decryptStickyCookie(sticky)

		// Check sticky session
		if sHost != "" && sPort != 0 {
			for _, droplet := range s {
				if droplet.Host == sHost && droplet.Port == sPort {
					rm = droplet
					break
				}
			}
		}
	}

	// No valid sticky session found, choose one randomly
	if rm == nil {
		rm = s[rand.Intn(len(s))]
	}

	return rm
}

func (p *Proxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// Return 200 OK for heartbeats from LB
	if req.UserAgent() == "HTTP-Monitor/1.1" {
		rw.WriteHeader(http.StatusOK)
		fmt.Fprintln(rw, "ok")
		return
	}

	p.varz.IncRequests()

	r := p.Lookup(req)
	if r == nil {
		p.recordStatus(400, start, nil)
		p.varz.IncBadRequests()

		rw.WriteHeader(http.StatusNotFound)
		return
	}

	// Save the app_id of active app
	p.activeApps.Insert(r.App)

	p.varz.IncRequestsWithTags(r.Tags)
	p.varz.IncAppRequests(getUrl(req))

	outreq := new(http.Request)
	*outreq = *req // includes shallow copies of maps, but okay

	outHost := fmt.Sprintf("%s:%d", r.Host, r.Port)
	outreq.URL.Scheme = "http"
	outreq.URL.Host = outHost

	outreq.Proto = "HTTP/1.1"
	outreq.ProtoMajor = 1
	outreq.ProtoMinor = 1
	outreq.Close = false

	// Remove the connection header to the backend.  We want a
	// persistent connection, regardless of what the client sent
	// to us.  This is modifying the same underlying map from req
	// (shallow copied above) so we only copy it if necessary.
	if outreq.Header.Get("Connection") != "" {
		outreq.Header = make(http.Header)
		copyHeader(outreq.Header, req.Header)
		outreq.Header.Del("Connection")
	}

	if clientIp, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		outreq.Header.Set("X-Forwarded-For", clientIp)
	}

	res, err := http.DefaultTransport.RoundTrip(outreq)
	if err != nil {
		log.Errorf("http: proxy error: %v", err)

		p.recordStatus(500, start, r.Tags)
		p.varz.IncBadRequests()

		rw.WriteHeader(http.StatusInternalServerError)
		return
	}

	p.recordStatus(res.StatusCode, start, r.Tags)

	copyHeader(rw.Header(), res.Header)

	if req.Header.Get(VcapTraceHeader) != "" {
		rw.Header().Set(VcapRouterHeader, config.ip)
		rw.Header().Set(VcapBackendHeader, outHost)
	}

	needSticky := false
	for _, v := range res.Cookies() {
		if v.Name == StickyCookieKey {
			needSticky = true
			break
		}
	}

	if needSticky {
		cookie := &http.Cookie{
			Name:  VcapCookieId,
			Value: p.se.getStickyCookie(r),
		}
		http.SetCookie(rw, cookie)
	}

	rw.WriteHeader(res.StatusCode)

	if res.Body != nil {
		var dst io.Writer = rw
		io.Copy(dst, res.Body)
	}
}

func (p *Proxy) recordStatus(status int, start time.Time, tags map[string]string) {
	latency := int(time.Since(start).Nanoseconds() / 1000000)
	p.varz.RecordResponse(status, latency, tags)
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func getUrl(req *http.Request) string {
	host := req.Host

	// Remove :<port>
	i := strings.Index(host, ":")
	if i >= 0 {
		host = host[0:i]
	}

	return strings.ToLower(host)
}