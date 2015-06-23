package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"skipper/skipper"
)

const defaultSettingsBufferSize = 32
const proxyBufferSize = 8192
const proxyErrorFmt = "proxy: %s"

type flusherWriter interface {
	http.Flusher
	io.Writer
}

type proxy struct {
	settings  <-chan skipper.Settings
	transport *http.Transport
}

type filterContext struct {
	w   http.ResponseWriter
	req *http.Request
	res *http.Response
}

func proxyError(m string) error {
	return fmt.Errorf(proxyErrorFmt, m)
}

func copyHeader(to, from http.Header) {
	for k, v := range from {
		to[http.CanonicalHeaderKey(k)] = v
	}
}

func cloneHeader(h http.Header) http.Header {
	hh := make(http.Header)
	copyHeader(hh, h)
	return hh
}

func copyStream(to flusherWriter, from io.Reader) error {
	for {
		b := make([]byte, proxyBufferSize)

		l, rerr := from.Read(b)
		if rerr != nil && rerr != io.EOF {
			return rerr
		}

		_, werr := to.Write(b[:l])
		if werr != nil {
			return werr
		}

		to.Flush()

		if rerr == io.EOF {
			return nil
		}
	}
}

func mapRequest(r *http.Request, b skipper.Backend) (*http.Request, error) {
	if b == nil {
		return nil, proxyError("missing backend")
	}

	rr, err := http.NewRequest(r.Method, b.Url(), r.Body)
	if err != nil {
		return nil, err
	}

	rr.Header = cloneHeader(r.Header)
	return rr, nil
}

func getSettingsBufferSize() int {
	// todo: return defaultFeedBufferSize when not dev env
	return 0
}

func Make(sd skipper.SettingsSource) http.Handler {
	sc := make(chan skipper.Settings, getSettingsBufferSize())
	sd.Subscribe(sc)
	return &proxy{sc, &http.Transport{}}
}

func applyFilterSafe(f skipper.Filter, p func()) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("middleware", f.Id(), err)
		}
	}()

	p()
}

func applyFiltersToRequest(f []skipper.Filter, ctx skipper.FilterContext) {
	for _, fi := range f {
		applyFilterSafe(fi, func() {
			fi.Request(ctx)
		})
	}
}

func applyFiltersToResponse(f []skipper.Filter, ctx skipper.FilterContext) {
	for i, _ := range f {
		fi := f[len(f)-1-i]
		applyFilterSafe(fi, func() {
			fi.Response(ctx)
		})
	}
}

func (c *filterContext) ResponseWriter() http.ResponseWriter {
	return c.w
}

func (c *filterContext) Request() *http.Request {
	return c.req
}

func (c *filterContext) Response() *http.Response {
	return c.res
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hterr := func(err error) {
		// todo: just a bet that we shouldn't send here 50x
		http.Error(w, http.StatusText(404), 404)
		log.Println(err)
	}

	s := <-p.settings
	if s == nil {
		hterr(proxyError("missing settings"))
		return
	}

	rt, err := s.Route(r)
	if rt == nil || err != nil {
		// todo: we need a somewhat more extensive logging here
		hterr(proxyError(fmt.Sprintf("routing failed: %v %v", r.URL, err)))
		return
	}

	f := rt.Filters()
	c := &filterContext{w, r, nil}
	applyFiltersToRequest(f, c)

	rr, err := mapRequest(r, rt.Backend())
	if err != nil {
		hterr(err)
		return
	}

	rs, err := p.transport.RoundTrip(rr)
	if err != nil {
		hterr(err)
		return
	}

	c.res = rs
	applyFiltersToResponse(f, c)

	defer func() {
		err = rs.Body.Close()
		if err != nil {
			log.Println(err)
		}
	}()

	copyHeader(w.Header(), rs.Header)
	w.WriteHeader(rs.StatusCode)
	copyStream(w.(flusherWriter), rs.Body)
}