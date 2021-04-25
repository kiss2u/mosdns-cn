//     Copyright (C) 2020-2021, IrineSistiana
//
//     This file is part of mosdns.
//
//     mosdns is free software: you can redistribute it and/or modify
//     it under the terms of the GNU General Public License as published by
//     the Free Software Foundation, either version 3 of the License, or
//     (at your option) any later version.
//
//     mosdns is distributed in the hope that it will be useful,
//     but WITHOUT ANY WARRANTY; without even the implied warranty of
//     MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//     GNU General Public License for more details.
//
//     You should have received a copy of the GNU General Public License
//     along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"github.com/IrineSistiana/mosdns/dispatcher/mlog"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/cache/mem_cache"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/matcher/domain"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/matcher/msg_matcher"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/matcher/netlist"
	_ "github.com/IrineSistiana/mosdns/dispatcher/pkg/matcher/v2data"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/server"
	"github.com/IrineSistiana/mosdns/dispatcher/pkg/upstream"
	"github.com/jessevdk/go-flags"
	"go.uber.org/zap"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev/unknown"

var Opts struct {
	ServerAddr     string   `short:"s" long:"server" description:"Server address" required:"true"`
	CacheSize      int      `short:"c" long:"cache" description:"Cache size"`
	LocalUpstream  []string `long:"local-upstream" description:"Local upstream" required:"true"`
	LocalIP        []string `long:"local-ip" description:"Local ip" required:"true"`
	LocalDomain    []string `long:"local-domain" description:"Local domain"`
	LocalLatency   int      `long:"local-latency" description:"Local latency in milliseconds" default:"50"`
	RemoteUpstream []string `long:"remote-upstream" description:"Remote upstream" required:"true"`
	RemoteDomain   []string `long:"remote-domain" description:"Remote domain"`
	Debug          bool     `short:"v" long:"debug" description:"Verbose log"`
}

func main() {
	_, err := flags.Parse(&Opts)
	if err != nil { // error msg has been printed by flags
		os.Exit(1)
	}

	go run()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill, syscall.SIGTERM)
	s := <-c
	mlog.S().Infof("%s, exiting", s)
	os.Exit(0)
}

func run() {
	if Opts.Debug {
		mlog.Level().SetLevel(zap.DebugLevel)
	} else {
		mlog.Level().SetLevel(zap.InfoLevel)
	}

	h := new(cnHandler)

	if Opts.CacheSize > 8 {
		h.cache = mem_cache.NewMemCache(8, Opts.CacheSize/8, time.Minute)
	}

	for i, s := range Opts.LocalUpstream {
		addr, socks5, netAddr, err := splitArgs(s)
		if err != nil {
			mlog.S().Fatalf("Failed to parse local upstream #%d: %v", i, err)
		}

		fu, err := upstream.NewFastUpstream(addr, upstream.WithSocks5(socks5), upstream.WithDialAddr(netAddr))
		if err != nil {
			mlog.S().Fatalf("Failed to load local upstream #%d: %v", i, err)
		}

		trusted := false
		if i == 0 {
			trusted = true
		}
		h.localUpstream = append(h.localUpstream, wrapFU(fu, trusted))
	}

	for i, s := range Opts.RemoteUpstream {
		addr, socks5, netAddr, err := splitArgs(s)
		if err != nil {
			mlog.S().Fatalf("Failed to parse remote upstream #%d: %v", i, err)
		}

		fu, err := upstream.NewFastUpstream(addr, upstream.WithSocks5(socks5), upstream.WithDialAddr(netAddr))
		if err != nil {
			mlog.S().Fatalf("Failed to load remote upstream #%d: %v", i, err)
		}

		trusted := false
		if i == 0 {
			trusted = true
		}
		h.remoteUpstream = append(h.remoteUpstream, wrapFU(fu, trusted))
	}

	if len(Opts.LocalDomain) > 0 {
		mixMatcher := domain.NewMixMatcher(domain.WithDomainMatcher(domain.NewSimpleDomainMatcher()))
		if err := batchLoadDomainFile(mixMatcher, Opts.LocalDomain); err != nil {
			mlog.S().Fatalf("Failed to load local domain: %v", err)
		}
		mlog.S().Infof("Local domain matcher loaded, length: %d", mixMatcher.Len())
		h.localDomain = msg_matcher.NewQNameMatcher(mixMatcher)
	}

	if len(Opts.RemoteDomain) > 0 {
		mixMatcher := domain.NewMixMatcher(domain.WithDomainMatcher(domain.NewSimpleDomainMatcher()))
		if err := batchLoadDomainFile(mixMatcher, Opts.RemoteDomain); err != nil {
			mlog.S().Fatalf("Failed to load remote domain: %v", err)
		}
		mlog.S().Infof("Remote domain matcher loaded, length: %d", mixMatcher.Len())
		h.remoteDomain = msg_matcher.NewQNameMatcher(mixMatcher)
	}

	// Opts.LocalIP is required
	nl := netlist.NewList()
	if err := batchLoadIPFile(nl, Opts.LocalIP); err != nil {
		mlog.S().Fatalf("Failed to load local ip: %v", err)
	}
	nl.Sort()
	mlog.S().Infof("Local IP matcher loaded, length: %d", nl.Len())
	h.localIP = msg_matcher.NewAAAAAIPMatcher(nl)

	h.localLatency = time.Millisecond * time.Duration(Opts.LocalLatency)

	udpServer := server.NewServer("udp", Opts.ServerAddr, server.WithHandler(h))
	tcpServer := server.NewServer("tcp", Opts.ServerAddr, server.WithHandler(h))

	go func() {
		err := udpServer.Start()
		if err != nil {
			mlog.S().Fatalf("udp server exited: %v", err)
		}
	}()

	go func() {
		err := tcpServer.Start()
		if err != nil {
			mlog.S().Fatalf("tcp server exited: %v", err)
		}
	}()

	select {}
}

func splitArgs(s string) (addr string, socks5 string, netAddr string, err error) {
	// parse protocol and server addr
	if !strings.Contains(s, "://") {
		s = "udp://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid upstream address, %w", err)
	}
	v := u.Query()
	socks5 = v.Get("socks5")
	netAddr = v.Get("netaddr")
	u.RawQuery = ""
	addr = u.String()
	return addr, socks5, netAddr, nil
}

func batchLoadDomainFile(m *domain.MixMatcher, files []string) error {
	for _, f := range files {
		if err := domain.LoadFromFile(m, f, nil); err != nil {
			return err
		}
	}
	return nil
}

func batchLoadIPFile(l *netlist.List, files []string) error {
	for _, f := range files {
		if err := netlist.LoadFromFile(l, f); err != nil {
			return err
		}
	}
	return nil
}
