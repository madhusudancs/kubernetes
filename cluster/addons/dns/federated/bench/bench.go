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

// Measure proxy delay
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

var (
	hostname    = flag.String("host", "kubernetes", "host name to lookup")
	federatedns = flag.String("federatedns", "", "IP address of the federated nameserver")
	iterations  = flag.Int("iterations", 100, "number of times you want the loop to be executed")
)

func main() {
	flag.Parse()
	defaultServer(*hostname)
	federatedServer(*hostname)
}

func defaultServer(host string) {
	total := int64(0)
	n := int64(0)
	for i := 0; i < *iterations; i++ {
		start := time.Now()
		addrs, err := net.LookupHost(host)
		end := time.Now()
		if err != nil {
			log.Printf("Error looking up host %s: %v", host, err)
			continue
		}
		if len(addrs) > 0 {
			total += end.Sub(start).Nanoseconds()
			n++
		}
	}
	fmt.Printf("Default server: Average time - %d/%d: %d\n", total, n, total/n)
}

func federatedServer(host string) {
	changeDNSServer()
	total := int64(0)
	n := int64(0)
	for i := 0; i < *iterations; i++ {
		start := time.Now()
		addrs, err := net.LookupHost(host)
		end := time.Now()
		if err != nil {
			log.Printf("Error looking up host %s: %v", host, err)
			continue
		}
		if len(addrs) > 0 {
			total += end.Sub(start).Nanoseconds()
			n++
		}
	}
	fmt.Printf("Federated server: Average time - %d/%d: %d\n", total, n, total/n)
}

func changeDNSServer() {
	resolvConf := "/etc/resolv.conf"
	stat, err := os.Stat(resolvConf)
	if err != nil {
		log.Fatalf("Could not stat %s: %v", resolvConf, err)
	}
	b, err := ioutil.ReadFile(resolvConf)
	if err != nil {
		log.Fatalf("Could not read %s: %v", resolvConf, err)
	}

	splits := strings.Split(string(b), "\n")
	if strings.HasPrefix(splits[1], "nameserver") {
		splits[1] = fmt.Sprintf("nameserver %s", *federatedns)
		c := strings.Join(splits, "\n")
		err := ioutil.WriteFile(resolvConf, []byte(c), stat.Mode())
		if err != nil {
			log.Fatalf("Could not write to %s: %v", resolvConf, err)
		}
	}
}
