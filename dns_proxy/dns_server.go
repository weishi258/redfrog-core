package dns_proxy

import (
	"context"
	"fmt"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/weishi258/redfrog-core/config"
	"github.com/weishi258/redfrog-core/log"
	"github.com/weishi258/redfrog-core/pac"
	"github.com/weishi258/redfrog-core/proxy_client"
	"github.com/weishi258/redfrog-core/routing"
	"go.uber.org/zap"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)


type dnsResolver struct {
	addr   string
	client *dns.Client
}

type DnsServer struct {
	routingMgr *routing.RoutingMgr
	pacMgr     *pac.PacListMgr
	server     *dns.Server

	localResolver  []*dnsResolver
	remoteResolver []*dnsResolver

	proxyClient *proxy_client.ProxyClient

	dnsResolverMux sync.RWMutex


	sendNum   int32
	dnsCaches *dnsCache
	dnsCacheMux sync.Mutex

	timeout time.Duration
}

type dnsCacheEntry struct {
	response *dns.Msg
	halfTtl  time.Time
	ttl      time.Time
}

type dnsCache struct {
	caches  map[string]*dnsCacheEntry
}

func (c *DnsServer) AddDnsCache(domain string, response *dns.Msg, ttl uint32) {
	c.dnsCacheMux.Lock()
	defer c.dnsCacheMux.Unlock()
	if c.dnsCaches != nil{
		c.dnsCaches.caches[domain] = &dnsCacheEntry{response: response, halfTtl: time.Now().Add(time.Duration(ttl >> 1) * time.Second), ttl: time.Now().Add(time.Duration(ttl) * time.Second)}
	}
}

func (c *DnsServer) DelDnsCache(domain string) {
	c.dnsCacheMux.Lock()
	defer c.dnsCacheMux.Unlock()
	if c.dnsCaches != nil{
		delete(c.dnsCaches.caches, domain)
	}

}

func (c *DnsServer) GetDnsCache(domain string) (*dns.Msg, bool) {
	c.dnsCacheMux.Lock()
	defer c.dnsCacheMux.Unlock()
	if c.dnsCaches != nil{
		if res, ok := c.dnsCaches.caches[domain]; ok {
			log.GetLogger().Debug("Get cache hit", zap.String("domain", domain))
			now := time.Now()
			if now.Before(res.ttl) {
				// we used halfTtl as an test to determine if we need to refresh the cache
				// it the current time + timeout > current time we will need to refresh cache even we hit cache to minimize dns lost
				return res.response, now.After(res.halfTtl)
			} else {
				delete(c.dnsCaches.caches, domain)
			}
		}
	}

	return nil, false
}

func StartDnsServer(dnsConfig config.DnsConfig, pacMgr *pac.PacListMgr, routingMgr *routing.RoutingMgr, proxyClient *proxy_client.ProxyClient) (ret *DnsServer, err error) {
	logger := log.GetLogger()

	ret = &DnsServer{}
	ret.proxyClient = proxyClient
	if routingMgr == nil {
		return nil, errors.New("Routing manager is nil")
	}
	ret.routingMgr = routingMgr

	if pacMgr == nil {
		return nil, errors.New("Pac list manager is nil")
	}
	ret.pacMgr = pacMgr

	ret.server = &dns.Server{Addr: dnsConfig.ListenAddr, Net: "udp", Handler: ret}
	logger.Info("Dns server starting", zap.String("addr", dnsConfig.ListenAddr))
	go func() {
		if err = ret.server.ListenAndServe(); err != nil {
			logger.Error("Dns server start failed", zap.String("error", err.Error()))
		}
	}()

	// create dns exchange client
	ret.localResolver = make([]*dnsResolver, 0)
	for _, addr := range dnsConfig.LocalResolver {
		var resolver *dnsResolver
		if strings.Index(addr, ":") >= 0 {
			resolver = &dnsResolver{addr, &dns.Client{Net: "udp"}}
		} else {
			resolver = &dnsResolver{fmt.Sprintf("%s:53", addr), &dns.Client{Net: "udp"}}
		}
		ret.localResolver = append(ret.localResolver, resolver)
		logger.Debug("DNS local resolver", zap.String("addr", resolver.addr))
	}

	ret.remoteResolver = make([]*dnsResolver, 0)
	for _, addr := range dnsConfig.ProxyResolver {
		var resolver *dnsResolver
		if strings.Index(addr, ":") >= 0 {
			resolver = &dnsResolver{addr, &dns.Client{Net: "udp"}}
		} else {
			resolver = &dnsResolver{fmt.Sprintf("%s:53", addr), &dns.Client{Net: "udp"}}
		}
		ret.remoteResolver = append(ret.remoteResolver, resolver)
		logger.Debug("DNS proxy resolver", zap.String("addr", resolver.addr))
	}

	if dnsConfig.Cache {
		logger.Info("Enable DNS cache")
		ret.dnsCaches = &dnsCache{caches: make(map[string]*dnsCacheEntry)}
	}
	ret.sendNum = int32(dnsConfig.SendNum)
	if ret.sendNum < 1 {
		ret.sendNum = 1
	}
	ret.timeout = time.Duration(dnsConfig.Timeout) * time.Second
	logger.Info("Set DNS send number", zap.Int("num", dnsConfig.SendNum))
	return
}
func (c *DnsServer)Reload(dnsConfig config.DnsConfig){
	logger := log.GetLogger()

	// reload resolver

	localResolver := make([]*dnsResolver, 0)
	for _, addr := range dnsConfig.LocalResolver {
		var resolver *dnsResolver
		if strings.Index(addr, ":") >= 0 {
			resolver = &dnsResolver{addr, &dns.Client{Net: "udp"}}
		} else {
			resolver = &dnsResolver{fmt.Sprintf("%s:53", addr), &dns.Client{Net: "udp"}}
		}
		localResolver = append(localResolver, resolver)
		logger.Debug("DNS local resolver", zap.String("addr", resolver.addr))
	}

	remoteResolver := make([]*dnsResolver, 0)
	for _, addr := range dnsConfig.ProxyResolver {
		var resolver *dnsResolver
		if strings.Index(addr, ":") >= 0 {
			resolver = &dnsResolver{addr, &dns.Client{Net: "udp"}}
		} else {
			resolver = &dnsResolver{fmt.Sprintf("%s:53", addr), &dns.Client{Net: "udp"}}
		}
		remoteResolver = append(remoteResolver, resolver)
		logger.Debug("DNS proxy resolver", zap.String("addr", resolver.addr))
	}
	c.dnsResolverMux.Lock()
	defer c.dnsResolverMux.Unlock()
	c.localResolver = localResolver
	c.remoteResolver = remoteResolver



	// reload DNS cache
	c.dnsCacheMux.Lock()
	defer c.dnsCacheMux.Unlock()

	if dnsConfig.Cache{
		if c.dnsCaches == nil{
			logger.Info("Enable DNS cache")
			c.dnsCaches = &dnsCache{caches: make(map[string]*dnsCacheEntry)}
		}
	}else{
		if c.dnsCaches != nil{
			logger.Info("Disable DNS cache")
			c.dnsCaches = nil
		}

	}

	// reload Send Num
	sendNum := dnsConfig.SendNum
	if sendNum < 1{
		sendNum = 1
	}
	atomic.StoreInt32(&c.sendNum, int32(sendNum))
	logger.Info("Set DNS send number", zap.Int("num", sendNum))

	logger.Info("Reload DNS config successful")
}

func (c *DnsServer) Stop() {
	logger := log.GetLogger()

	if err := c.server.Shutdown(); err != nil {
		logger.Error("Stop DNS server failed", zap.String("error", err.Error()))
	}

	logger.Info("Dns server stopped")
}

func (c *DnsServer) getResolver(bIsRemote bool) *dnsResolver {
	c.dnsResolverMux.RLock()
	defer c.dnsResolverMux.RUnlock()
	if bIsRemote {
		length := len(c.remoteResolver)
		if length == 1 {
			return c.remoteResolver[0]
		} else {
			return c.remoteResolver[rand.Int31n(int32(length))]
		}
	} else {
		length := len(c.localResolver)
		if length == 1 {
			return c.localResolver[0]
		} else {
			return c.localResolver[rand.Int31n(int32(length))]
		}
	}
}

func (c *DnsServer) applyFilterChain(r *dns.Msg) *dns.Msg {
	// TODO
	// 1. Implement DNS cache filter for fast performance
	// 2. Implement DNS block filter for ads blocking etc

	return nil
}

func (c *DnsServer) checkCache(r *dns.Msg) (*dns.Msg, bool) {
	if c.dnsCaches != nil {
		for _, q := range r.Question {
			if q.Qclass == dns.ClassINET {
				if resDns, needRefreshCache := c.GetDnsCache(strings.TrimSuffix(q.Name, ".")); resDns != nil {
					resDns.Id = r.Id
					return resDns, needRefreshCache
				}
			}
		}
	}
	return nil, false
}

//func (c * DnsServer) WriteBackProxyResponse(w dns.ResponseWriter, domainName string, responseBytes []byte){
//	logger := log.GetLogger()
//	resDns := new(dns.Msg)
//	if err := resDns.Unpack(responseBytes); err != nil {
//		logger.Error("DNS unpack for proxy resolver failed", zap.String("error", err.Error()))
//		return
//	}
//
//	shouldAddCache := false
//	var ttl uint32
//	for _, a := range resDns.Answer {
//		if a.Header().Class == dns.ClassINET {
//			if a.Header().Rrtype == dns.TypeA {
//				shouldAddCache = true
//				name := strings.TrimSuffix(a.Header().Name, ".")
//				c.routingMgr.AddIp(name, a.(*dns.A).A)
//				logger.Debug("ipv4 ip query", zap.String("domain", name), zap.String("ip", a.(*dns.A).A.String()))
//			} else if a.Header().Rrtype == dns.TypeAAAA {
//				shouldAddCache = true
//				name := strings.TrimSuffix(a.Header().Name, ".")
//				c.routingMgr.AddIp(name, a.(*dns.AAAA).AAAA)
//				logger.Debug("ipv6 ip query", zap.String("domain", name), zap.String("ip", a.(*dns.AAAA).AAAA.String()))
//			} else if a.Header().Rrtype == dns.TypeCNAME {
//				cname := strings.TrimSuffix(a.(*dns.CNAME).Target, ".")
//				c.pacMgr.AddDomain(cname)
//				logger.Debug("Add CNAME to list", zap.String("CNAME", cname))
//			}
//			if a.Header().Ttl > ttl{
//				ttl = a.Header().Ttl
//			}
//		}
//	}
//	if shouldAddCache && c.dnsCaches != nil {
//		c.AddDnsCache(domainName, responseBytes, ttl)
//	}
//
//	w.WriteMsg(resDns)
//}

func (c *DnsServer) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	logger := log.GetLogger()

	if resDns := c.applyFilterChain(r); resDns != nil {
		w.WriteMsg(resDns)
		return
	}

	isBlacked := false
	var domainName string
	for _, q := range r.Question {
		name := strings.TrimSuffix(q.Name, ".")
		if c.pacMgr.CheckDomain(name) {
			isBlacked = true
			domainName = name
			break
		}
	}

	if isBlacked {
		bWriteBack := false
		if resDns, bRefreshCache := c.checkCache(r); resDns != nil {
			w.WriteMsg(resDns)
			// we don't need to refresh dns
			if !bRefreshCache{
				return
			}
			bWriteBack = true
		}

		resolver := c.getResolver(true)
		data, err := r.Pack()
		if err != nil {
			logger.Error("Pack DNS query for proxy failed", zap.String("error", err.Error()))
			return
		}
		resDns, err := c.proxyClient.ExchangeDNS(resolver.addr, data, c.timeout)
		if err != nil {
			logger.Error("DNS proxy resolve failed", zap.String("domain", domainName), zap.String("error", err.Error()))
			return
		}
		resDns.Id = r.Id
		shouldAddCache := false
		var ttl uint32
		for _, a := range resDns.Answer {
			if a.Header().Class == dns.ClassINET {
				if a.Header().Ttl > ttl{
					ttl = a.Header().Ttl
				}
				if a.Header().Rrtype == dns.TypeA {
					shouldAddCache = true
					name := strings.TrimSuffix(a.Header().Name, ".")
					c.routingMgr.AddIp(name, a.(*dns.A).A)
					logger.Debug("ipv4 ip query", zap.String("domain", name), zap.String("ip", a.(*dns.A).A.String()), zap.Uint32("ttl", ttl))

				// ipv6 is not fully support yet, so ignore now
				//} else if a.Header().Rrtype == dns.TypeAAAA {

				//	//shouldAddCache = true
				//	name := strings.TrimSuffix(a.Header().Name, ".")
				//	c.routingMgr.AddIp(name, a.(*dns.AAAA).AAAA)
				//	logger.Debug("ipv6 ip query", zap.String("domain", name), zap.String("ip", a.(*dns.AAAA).AAAA.String()), zap.Uint32("ttl", ttl))
				} else if a.Header().Rrtype == dns.TypeCNAME {
					cname := strings.TrimSuffix(a.(*dns.CNAME).Target, ".")
					c.pacMgr.AddDomain(cname)
					logger.Debug("Add CNAME to list", zap.String("CNAME", cname))
				}

			}
		}
		if shouldAddCache && c.dnsCaches != nil {
			c.AddDnsCache(domainName, resDns, ttl)
		}

		if !bWriteBack{
			w.WriteMsg(resDns)
		}

	} else {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()

		resolver := c.getResolver(false)
		if response, t, err := resolver.client.ExchangeContext(ctx, r, resolver.addr); err != nil {
			logger.Debug("Can not exchange dns query for local resolver", zap.String("addr", resolver.addr), zap.String("error", err.Error()))
		} else {
			logger.Debug("Dns query for local resolver successful", zap.String("addr", resolver.addr), zap.Duration("time", t))
			w.WriteMsg(response)
		}
	}

}
