// Package dns is oakd's resolver for the .internal zone: <app>.internal
// resolves to that app's machine IPs, everything else is forwarded upstream.
package dns

import (
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// zone is the suffix oak resolves itself; everything outside it forwards to
// Upstream.
const zone = ".internal."

// defaultUpstream is used when Upstream is empty.
const defaultUpstream = "1.1.1.1:53"

// answerTTL is deliberately short: machine IPs change on every deploy.
const answerTTL = 5

// Server answers DNS over UDP for the .internal zone and forwards
// everything else.
type Server struct {
	// Addr is the address to listen on, e.g. "10.200.0.1:53".
	Addr string

	// Resolve returns the current IPs for app, or nil if the app doesn't
	// exist (which the server turns into NXDOMAIN).
	Resolve func(app string) []net.IP

	// Upstream is the DNS server non-.internal queries are forwarded to.
	// Defaults to 1.1.1.1:53 when empty.
	Upstream string
}

// ListenAndServe opens a UDP socket on Addr and serves until it errors or is
// closed.
func (s *Server) ListenAndServe() error {
	conn, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		return fmt.Errorf("dns: listen %s: %w", s.Addr, err)
	}
	return s.Serve(conn)
}

// Serve answers DNS queries on an already-open connection. Tests use this to
// listen on 127.0.0.1:0 and learn the assigned port before serving.
func (s *Server) Serve(conn net.PacketConn) error {
	srv := &dns.Server{PacketConn: conn, Handler: s}
	return srv.ActivateAndServe()
}

// ServeDNS implements dns.Handler.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) != 1 {
		reply := new(dns.Msg)
		reply.SetRcode(r, dns.RcodeFormatError)
		_ = w.WriteMsg(reply)
		return
	}

	q := r.Question[0]
	if !strings.HasSuffix(strings.ToLower(q.Name), zone) {
		s.forward(w, r)
		return
	}
	s.resolveLocal(w, r, q)
}

func (s *Server) resolveLocal(w dns.ResponseWriter, r *dns.Msg, q dns.Question) {
	reply := new(dns.Msg)
	reply.SetReply(r)
	reply.Authoritative = true

	app := strings.TrimSuffix(strings.ToLower(q.Name), zone)
	ips := s.Resolve(app)
	if len(ips) == 0 {
		// The app genuinely doesn't exist: NXDOMAIN, for any query type.
		reply.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(reply)
		return
	}

	// The name exists. For any query other than A (e.g. the AAAA probe that
	// resolvers like Erlang's inet_res -- Postgrex -- send as a matter of
	// course), answer NOERROR with an empty answer section (NODATA), not
	// NXDOMAIN: returning NXDOMAIN for a name that exists makes those
	// resolvers conclude the host is unknown and give up, ignoring the A
	// record that's right here. Only A carries data in this zone (guests are
	// IPv4-only on oak0).
	if q.Qtype != dns.TypeA {
		_ = w.WriteMsg(reply)
		return
	}

	for _, ip := range ips {
		reply.Answer = append(reply.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: answerTTL},
			A:   ip,
		})
	}
	_ = w.WriteMsg(reply)
}

func (s *Server) forward(w dns.ResponseWriter, r *dns.Msg) {
	upstream := s.Upstream
	if upstream == "" {
		upstream = defaultUpstream
	}

	resp, err := dns.Exchange(r, upstream)
	if err != nil {
		reply := new(dns.Msg)
		reply.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(reply)
		return
	}
	_ = w.WriteMsg(resp)
}
