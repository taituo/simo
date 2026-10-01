package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ServeStdio reads newline-delimited JSON-RPC messages from r and writes
// responses to w, one per line, until r ends or ctx is cancelled. Messages
// are handled in order.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16<<20) // seeds can be long
	var mu sync.Mutex
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp := s.HandleMessage(ctx, sc.Bytes())
		if resp == nil {
			continue
		}
		mu.Lock()
		_, err := w.Write(append(resp, '\n'))
		mu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// HTTPHandler serves the streamable HTTP transport in its JSON response
// form: clients POST JSON-RPC messages and get JSON back. The server opens
// no event streams, so GET and DELETE answer 405, which the protocol
// allows. Requests carrying an Origin header that is not a loopback host
// are refused, to stop DNS-rebinding attacks from web pages.
func (s *Server) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !loopbackOrigin(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "this server answers POST only (no event streams)", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := s.HandleMessage(r.Context(), body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})
}

func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
