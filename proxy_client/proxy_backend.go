package proxy_client

import (
	"bytes"
	"fmt"
	"github.com/pkg/errors"
	"github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/weishi258/redfrog-core/common"
	"github.com/weishi258/redfrog-core/config"
	"github.com/weishi258/redfrog-core/log"
	"github.com/weishi258/redfrog-core/network"
	"github.com/xtaci/smux"
	"go.uber.org/zap"
	"io"
	"net"
	"sync"
	"time"
)

type proxyBackend struct{
	cipher_      core.Cipher
	tcpAddr         net.TCPAddr
	udpAddr			*net.UDPAddr

	networkType_ 	string
	tcpTimeout_  	time.Duration
	udpTimeout_  	time.Duration
	udpNatMap_   	*udpNatMap
	dnsNatMap_	 	*dnsNatMap
	kcpBackend		*KCPBackend
}

type dnsNatMap struct {
	sync.RWMutex
	entries		map[string]net.PacketConn

}

func (c *dnsNatMap)Get(key string)net.PacketConn{
	c.RLock()
	defer c.RUnlock()
	ret, ok := c.entries[key]
	if ok{
		return ret
	}else{
		return nil
	}
}

func (c *dnsNatMap)Add(key string, conn net.PacketConn){
	c.Lock()
	defer c.Unlock()
	c.entries[key] = conn
}

func (c *dnsNatMap)Del(key string){
	c.Lock()
	defer c.Unlock()
	delete(c.entries, key)
}

type relayDataRes struct {
	outboundSize int64
	Err          error
}


type udpProxyEntry struct{
	src_    	net.PacketConn
	dst_     	net.PacketConn
	srcAddr_	*net.UDPAddr
	header_		[]byte
	timeout		time.Duration

}
func createUDPProxyEntry(src net.PacketConn, dst net.PacketConn, srcAddr *net.UDPAddr, dstAddr *net.UDPAddr, timeout time.Duration) (*udpProxyEntry, error) {
	addr, err := network.ConvertShadowSocksAddr(dstAddr.String())
	if err != nil{
		return nil, err
	}
	return &udpProxyEntry{src, dst, srcAddr, addr, timeout}, nil
}

func (c *udpProxyEntry) copyFromRemote() error{
	logger := log.GetLogger()
	buffer := make([]byte, common.UDP_BUFFER_SIZE)
	for {
		c.dst_.SetReadDeadline(time.Now().Add(c.timeout))
		n, _, err := c.dst_.ReadFrom(buffer)

		if err != nil{
			return err
		}
		logger.Debug("Read from remote", zap.Int("size", n))
		// should check header
		if n > len(c.header_) {
			logger.Debug("Write back to origin", zap.String("addr", c.srcAddr_.String()))
			if _, err = c.src_.WriteTo(buffer[len(c.header_):n], c.srcAddr_); err != nil{
				return err
			}
		}else{
			return errors.New(fmt.Sprintf("UDP Read too few bytes: %d", n))
		}

	}
}

type udpNatMap struct{
	sync.RWMutex
	entries		map[string]*udpProxyEntry
}

func (c *udpNatMap) Add(key string, entry *udpProxyEntry){
	c.Lock()
	defer c.Unlock()
	c.entries[key] = entry
}
func (c *udpNatMap) Del(key string){
	c.Lock()
	defer c.Unlock()
	delete(c.entries, key)
}
func (c *udpNatMap) Get(key string) *udpProxyEntry{
	c.RLock()
	defer c.RUnlock()
	if entry, ok := c.entries[key]; ok {
		return entry
	}else{
		return nil
	}
}

func computeUDPKey(src *net.UDPAddr, dst *net.UDPAddr) string{
	return fmt.Sprintf("%s->%s", src.String(), dst.String())
}


func CreateProxyBackend(config config.RemoteServerConfig, tcpTimeout int, udpTimeout int) (ret *proxyBackend, err error){

	ret = &proxyBackend{}
	ret.tcpTimeout_ = time.Second * time.Duration(tcpTimeout)
	ret.udpTimeout_ = time.Second * time.Duration(udpTimeout)

	var isIPv6 bool
	if isIPv6, err = network.CheckIPFamily(config.RemoteServer); err != nil{
		err = errors.Wrap(err, fmt.Sprintf("Invalid IP format: %s", config.RemoteServer))
		return
	}
	if isIPv6 {
		ret.networkType_ = "tcp6"
	}else{
		ret.networkType_ = "tcp4"
	}
	if ip, port, ee := network.ParseAddr(config.RemoteServer, isIPv6); ee != nil{
		err = errors.Wrap(ee, "Parse IPv4 failed")
		return
	}else{
		ret.tcpAddr = net.TCPAddr{IP: ip, Port: port}
		ret.udpAddr = &net.UDPAddr{IP: ip, Port: port}
	}


	if ret.cipher_, err = core.PickCipher(config.Crypt, []byte{}, config.Password); err != nil{
		err = errors.Wrap(err, "Generate cipher failed")
		return
	}

	ret.udpNatMap_ = &udpNatMap{entries: make(map[string]*udpProxyEntry)}
	ret.dnsNatMap_ = &dnsNatMap{entries: make(map[string]net.PacketConn)}

	if config.Kcptun.Enable{
		if ret.kcpBackend, err = StartKCPBackend(config.Kcptun, config.Kcptun.Crypt, config.Password); err != nil{
			err = errors.Wrap(err, "Create KCP backend failed")
		}
	}

	return
}

func (c *proxyBackend)Stop(){
	logger := log.GetLogger()

	c.udpNatMap_.Lock()
	defer c.udpNatMap_.Unlock()

	for _, entry := range c.udpNatMap_.entries{
		if err := entry.src_.Close(); err != nil{
			logger.Error("Close UDP proxy failed", zap.String("error", err.Error()))
		}
		if err := entry.dst_.Close(); err != nil{
			logger.Error("Close UDP proxy failed", zap.String("error", err.Error()))
		}
	}

	c.dnsNatMap_.Lock()
	defer c.dnsNatMap_.Unlock()

	for _, entry := range c.dnsNatMap_.entries{
		if err := entry.Close(); err != nil{
			logger.Error("Close DNS proxy failed", zap.String("error", err.Error()))
		}
	}

	if c.kcpBackend != nil{
		c.kcpBackend.Stop()
	}
	logger.Info("Proxy backend stopped")
}

func (c *proxyBackend) createTCPConn() (conn net.Conn, err error){

	conn, err = net.DialTCP(c.networkType_, nil, &c.tcpAddr)
	if err != nil{
		return
	}
	conn.(*net.TCPConn).SetKeepAlive(true)

	conn = c.cipher_.StreamConn(conn)

	return

}

func (c *proxyBackend)relayKCPData(srcConn net.Conn, kcpConn *smux.Stream, header []byte) (inboundSize int64, outboundSize int64, err error){
	defer kcpConn.Close()

	srcConn.SetWriteDeadline(time.Now().Add(c.tcpTimeout_))
	kcpConn.SetWriteDeadline(time.Now().Add(c.tcpTimeout_))

	if _, err = kcpConn.Write(header); err != nil{
		err = errors.Wrap(err, "Kcp write to remote server failed")
		return
	}

	ch := make(chan relayDataRes)

	go func(){
		res := relayDataRes{}
		res.outboundSize, res.Err = io.Copy(srcConn, kcpConn)
		srcConn.SetDeadline(time.Now())
		kcpConn.SetDeadline(time.Now())
		ch <- res
	}()

	inboundSize, err = io.Copy(kcpConn, srcConn)
	srcConn.SetDeadline(time.Now())
	kcpConn.SetDeadline(time.Now())
	rs := <- ch

	if err == nil{
		err = rs.Err
	}

	outboundSize = rs.outboundSize

	return
}

func (c *proxyBackend) RelayTCPData(src net.Conn) (inboundSize int64, outboundSize int64, err error){
	//logger := log.GetLogger()

	var originDst []byte
	if originDst, err = network.ConvertShadowSocksAddr(src.LocalAddr().String()); err != nil{
		err = errors.Wrap(err, "Parse origin dst failed")
		return
	}

	// try relay data through KCP is enabled and working
	if c.kcpBackend != nil	{
		// try to get an KCP steam connection, if not fall back to default proxy mode
		if kcpConn, err := c.kcpBackend.GetKcpConn(); err == nil{
			return c.relayKCPData(src, kcpConn, originDst)
		}
	}

	var dst net.Conn
	if dst, err = c.createTCPConn(); err != nil{
		err = errors.Wrap(err, "Create remote conn failed")
		return
	}
	defer dst.Close()

	// set deadline timeout
	dst.SetWriteDeadline(time.Now().Add(c.tcpTimeout_))
	src.SetWriteDeadline(time.Now().Add(c.tcpTimeout_))

	if _, err = dst.Write(originDst); err != nil{
		err = errors.Wrap(err, "Write to remote server failed")
		return
	}
	ch := make(chan relayDataRes)

	go func() {
		res := relayDataRes{}
		res.outboundSize, res.Err = io.Copy(dst, src)
		dst.SetDeadline(time.Now()) // wake up the other goroutine blocking on right
		src.SetDeadline(time.Now()) // wake up the other goroutine blocking on left
		ch <- res
	}()

	inboundSize, err = io.Copy(src, dst)
	dst.SetDeadline(time.Now()) // wake up the other goroutine blocking on right
	src.SetDeadline(time.Now()) // wake up the other goroutine blocking on left
	rs := <-ch

	if err == nil {
		err = rs.Err
	}

	outboundSize = rs.outboundSize
	return
}

func (c *proxyBackend) RelayUDPData(srcAddr *net.UDPAddr, dstAddr *net.UDPAddr, leakyBuffer *common.LeakyBuffer, data *bytes.Buffer, dataLen int) error{
	logger := log.GetLogger()

	udpKey := computeUDPKey(srcAddr, dstAddr)

	udpProxy := c.udpNatMap_.Get(udpKey)


	//logger.Debug("UDP relay ",zap.String("srcAddr", srcAddr.String()), zap.String("dstAddr", dstAddr.String()))
	if udpProxy == nil{
		dstConn, err := net.ListenPacket("udp", "")
		if err != nil{
			return errors.Wrap(err, "UDP proxy listen local failed")
		}
		dstConn = c.cipher_.PacketConn(dstConn)

		srcConn, err := network.DialTransparentUDP(dstAddr)
		if err != nil{
			dstConn.Close()
			return errors.Wrap(err, "UDP proxy listen using transparent failed")
		}
		if udpProxy, err = createUDPProxyEntry(srcConn, dstConn, srcAddr, dstAddr, c.udpTimeout_); err != nil{
			dstConn.Close()
			srcConn.Close()
			return errors.Wrap(err,"Create udp proxy entry failed")
		}

		// now lets run copy from dst
		go func(){
			c.udpNatMap_.Add(udpKey, udpProxy)
			if err := udpProxy.copyFromRemote(); err != nil{
				logger.Debug("Proxy UDP from dst to local stopped", zap.String("error", err.Error()))
			}
			c.udpNatMap_.Del(udpKey)

			udpProxy.src_.Close()
			udpProxy.dst_.Close()
		}()
	}

	// compose udp socks5 header
	udpProxy.dst_.SetReadDeadline(time.Now().Add(c.udpTimeout_))

	headerLen := len(udpProxy.header_)
	totalLen := headerLen + dataLen

	if totalLen > leakyBuffer.GetBufferSize(){
		// too big for our leakybuffer
		writeData := make([]byte, totalLen)
		copy(writeData[:headerLen], udpProxy.header_)
		copy(writeData[headerLen:totalLen], data.Bytes()[:dataLen])
		// set timeout for each send
		// write to remote shadowsocks server
		if _, err := udpProxy.dst_.WriteTo(writeData, c.udpAddr); err != nil{
			return err
		}

	}else{
		// get leaky buffer
		newBuffer := leakyBuffer.Get()
		defer leakyBuffer.Put(newBuffer)
		copy(newBuffer.Bytes(), udpProxy.header_)
		copy(newBuffer.Bytes()[headerLen: ], data.Bytes()[:dataLen])
		// set timeout for each send
		// write to remote shadowsocks server
		if _, err := udpProxy.dst_.WriteTo(newBuffer.Bytes()[:totalLen], c.udpAddr); err != nil{
			return err
		}
	}


	return nil
}

func (c *proxyBackend) RelayDNS(srcAddr string, dnsAddr string, data []byte, leakyBuffer *common.LeakyBuffer, dnsTimeout time.Duration) (response []byte, err error){
	//logger := log.GetLogger()


	addrBytes, err := network.ConvertShadowSocksAddr(dnsAddr)
	if err != nil{
		err = errors.Wrap(err, "DNS convert to shadowsocks addr failed")
		return
	}

	dataLen := len(data)
	addrLen := len(addrBytes)
	totalLen := addrLen + dataLen

	buffer := leakyBuffer.Get()
	defer leakyBuffer.Put(buffer)

	copy(buffer.Bytes(), addrBytes)
	copy(buffer.Bytes()[addrLen:], data)



	dstConn := c.dnsNatMap_.Get(srcAddr)
	if dstConn == nil{
		if dstConn, err = net.ListenPacket("udp", ""); err != nil{
			err = errors.Wrap(err, "UDP proxy listen local failed")
			return
		}
		dstConn = c.cipher_.PacketConn(dstConn)
		c.dnsNatMap_.Add(srcAddr, dstConn)
	}
	defer func(){
		c.dnsNatMap_.Del(srcAddr)
		dstConn.Close()
	}()

	// set timeout for each send
	// write to remote shadowsocks server

	if _, err = dstConn.WriteTo(buffer.Bytes()[:totalLen], c.udpAddr); err != nil{
		err = errors.Wrap(err, "Write to remote DNS failed")
		return
	}

	dstConn.SetReadDeadline(time.Now().Add(dnsTimeout))
	n, _, err := dstConn.ReadFrom(buffer.Bytes())
	if err != nil{
		return
	}
	if n <= addrLen{
		err = errors.New("Read DNS query empty")
		return
	}
	response = make([]byte, n)
	copy(response, buffer.Bytes()[addrLen:n])

	return

}