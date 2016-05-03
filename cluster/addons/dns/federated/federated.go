/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This is a DNS proxy
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

var (
	localnameserver = flag.String("localnameserver", "", "ip:port of the nameserver that's authoritative for cluster.local.")
	nameserver      = flag.String("nameserver", "", "ip:port of the nameserver that's authoritative for all other domains")
)

func main() {
	flag.Parse()

	s := newServer()
	s.run()
}

type server struct {
	group        *sync.WaitGroup
	dnsUDPclient *dns.Client // used for forwarding queries
	dnsTCPclient *dns.Client // used for forwarding queries
}

// New returns a new DNS server.
func newServer() *server {
	return &server{
		group:        new(sync.WaitGroup),
		dnsUDPclient: &dns.Client{Net: "udp", ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, SingleInflight: true},
		dnsTCPclient: &dns.Client{Net: "tcp", ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, SingleInflight: true},
	}
}

// Run is a blocking operation that starts the server listening on the DNS ports.
func (s *server) run() error {
	mux := dns.NewServeMux()
	mux.Handle(".", s)

	s.group.Add(1)
	go func() {
		defer s.group.Done()
		if err := dns.ListenAndServe("0.0.0.0:53", "tcp", mux); err != nil {
			log.Fatalf("%s", err)
		}
	}()

	s.group.Add(1)
	go func() {
		defer s.group.Done()
		if err := dns.ListenAndServe("0.0.0.0:53", "udp", mux); err != nil {
			log.Fatalf("%s", err)
		}
	}()

	s.group.Wait()
	return nil
}

// ServeDNS is the handler for DNS requests, responsible for parsing DNS request, possibly forwarding
// it to a real dns server and returning a response.
func (s *server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	resp, err := s.handle(req, isTCP(w))
	if err != nil {
		log.Printf("failure to forward stub request %q", err)
		m := s.serverFailure(req)
		w.WriteMsg(m)
		return
	}
	resp.Compress = true
	resp.Id = req.Id
	w.WriteMsg(resp)
}

func (s *server) handle(req *dns.Msg, isTCP bool) (*dns.Msg, error) {
	q := req.Question[0]
	name := strings.ToLower(q.Name)

	fsfx := ".federation.global."
	lsfx := ".cluster.local."

	log.Printf("Requested name: %s", name)
	if strings.HasSuffix(name, fsfx) {
		base := strings.TrimSuffix(name, fsfx)
		// Make a copy of the request
		lreq := *req
		lreq.Question[0].Name = base + lsfx

		lresp, err := s.forward(&lreq, *localnameserver, isTCP)
		gresp, err := s.forward(req, *nameserver, isTCP)
		lrespCopy := *lresp
		resp := &lrespCopy
		log.Printf("local response: %+v", lresp)
		log.Printf("global response: %+v", gresp)
		resp.Question = req.Question
		resp.Answer = append(resp.Answer, gresp.Answer...)
		resp.Ns = append(resp.Ns, gresp.Ns...)
		resp.Extra = append(resp.Extra, gresp.Extra...)
		return resp, err
	} else {
		return s.forward(req, *localnameserver, isTCP)
	}
}

func (s *server) forward(req *dns.Msg, nameserver string, isTCP bool) (*dns.Msg, error) {
	if isTCP {
		return exchangeWithRetry(s.dnsTCPclient, req, nameserver)
	} else {
		return exchangeWithRetry(s.dnsUDPclient, req, nameserver)
	}
	return nil, fmt.Errorf("invalid forward request")
}

func (s *server) serverFailure(req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(req, dns.RcodeServerFailure)
	return m
}

// isTCP returns true if the client is connecting over TCP.
func isTCP(w dns.ResponseWriter) bool {
	_, ok := w.RemoteAddr().(*net.TCPAddr)
	return ok
}

// exchangeWithRetry sends message m to server, but retries on ServerFailure.
func exchangeWithRetry(c *dns.Client, m *dns.Msg, server string) (*dns.Msg, error) {
	r, _, err := c.Exchange(m, server)
	if err == nil && r.Rcode == dns.RcodeServerFailure {
		// redo the query
		r, _, err = c.Exchange(m, server)
	}
	return r, err
}
