package socks5

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

var (
	errUnrecognizedAddrType = fmt.Errorf("Unrecognized address type")
)

// AddressRewriter is used to rewrite a destination transparently
type AddressRewriter interface {
	Rewrite(ctx context.Context, request *Request) (context.Context, *AddrSpec)
}

// A Request represents request received by a server
type Request struct {
	Header
	// AuthContext provided during negotiation
	AuthContext *AuthContext
	// LocalAddr of the the network server listen
	LocalAddr net.Addr
	// RemoteAddr of the the network that sent the request
	RemoteAddr net.Addr
	// DestAddr of the actual destination (might be affected by rewrite)
	DestAddr *AddrSpec
	// Reader connect of request
	Reader io.Reader
	// RawDestAddr of the desired destination
	RawDestAddr *AddrSpec
}

type conn interface {
	Write([]byte) (int, error)
	RemoteAddr() net.Addr
}

// NewRequest creates a new Request from the tcp connection
func NewRequest(bufConn io.Reader) (*Request, error) {
	/*
		The SOCKS request is formed as follows:
		+----+-----+-------+------+----------+----------+
		|VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
		+----+-----+-------+------+----------+----------+
		| 1  |  1  | X'00' |  1   | Variable |    2     |
		+----+-----+-------+------+----------+----------+
	*/
	hd, err := Parse(bufConn)
	if err != nil {
		return nil, err
	}
	if hd.Command != CommandConnect && hd.Command != CommandBind && hd.Command != CommandAssociate {
		return nil, fmt.Errorf("unrecognized command[%d]", hd.Command)
	}
	return &Request{
		Header:      hd,
		RawDestAddr: &hd.Address,
		Reader:      bufConn,
	}, nil
}

// handleRequest is used for request processing after authentication
func (s *Server) handleRequest(write io.Writer, req *Request) error {
	ctx := context.Background()

	// Resolve the address if we have a FQDN
	dest := req.RawDestAddr
	if dest.FQDN != "" {
		_ctx, addr, err := s.resolver.Resolve(ctx, dest.FQDN)
		if err != nil {
			if err := SendReply(write, req.Header, RepHostUnreachable); err != nil {
				return fmt.Errorf("failed to send reply, %v", err)
			}
			return fmt.Errorf("failed to resolve destination[%v], %v", dest.FQDN, err)
		}
		ctx = _ctx
		dest.IP = addr
	}

	// Apply any address rewrites
	req.DestAddr = req.RawDestAddr
	if s.rewriter != nil {
		ctx, req.DestAddr = s.rewriter.Rewrite(ctx, req)
	}

	// Check if this is allowed
	_ctx, ok := s.rules.Allow(ctx, req)
	if !ok {
		if err := SendReply(write, req.Header, RepRuleFailure); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("bind to %v blocked by rules", req.RawDestAddr)
	}
	ctx = _ctx

	// Switch on the command
	switch req.Command {
	case CommandConnect:
		if s.userConnectHandle != nil {
			return s.userConnectHandle(ctx, write, req)
		}
		return s.handleConnect(ctx, write, req)
	case CommandBind:
		if s.userBindHandle != nil {
			return s.userBindHandle(ctx, write, req)
		}
		return s.handleBind(ctx, write, req)
	case CommandAssociate:
		if s.userAssociateHandle != nil {
			return s.userAssociateHandle(ctx, write, req)
		}
		return s.handleAssociate(ctx, write, req)
	default:
		if err := SendReply(write, req.Header, RepCommandNotSupported); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("unsupported command[%v]", req.Command)
	}
}

// handleConnect is used to handle a connect command
func (s *Server) handleConnect(ctx context.Context, writer io.Writer, request *Request) error {
	// Attempt to connect
	dial := s.dial
	if dial == nil {
		dial = func(ctx context.Context, net_, addr string) (net.Conn, error) {
			return net.Dial(net_, addr)
		}
	}
	target, err := dial(ctx, "tcp", request.DestAddr.String())
	if err != nil {
		msg := err.Error()
		resp := RepHostUnreachable
		if strings.Contains(msg, "refused") {
			resp = RepConnectionRefused
		} else if strings.Contains(msg, "network is unreachable") {
			resp = RepNetworkUnreachable
		}
		if err := SendReply(writer, request.Header, resp); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("connect to %v failed, %v", request.RawDestAddr, err)
	}
	defer target.Close()

	// Send success
	if err := SendReply(writer, request.Header, RepSuccess, target.LocalAddr()); err != nil {
		return fmt.Errorf("failed to send reply, %v", err)
	}

	// Start proxying
	eCh1 := make(chan error, 1)
	eCh2 := make(chan error, 1)

	s.submit(func() { eCh1 <- s.Proxy(target, request.Reader) })
	s.submit(func() { eCh2 <- s.Proxy(writer, target) })
	// Wait
	select {
	case err = <-eCh1:
	case err = <-eCh2:
	}
	return err
}

// handleBind is used to handle a connect command
func (s *Server) handleBind(_ context.Context, writer io.Writer, request *Request) error {
	// TODO: Support bind
	if err := SendReply(writer, request.Header, RepCommandNotSupported); err != nil {
		return fmt.Errorf("failed to send reply: %v", err)
	}
	return nil
}

// handleAssociate is used to handle a connect command
func (s *Server) handleAssociate(ctx context.Context, writer io.Writer, request *Request) error {
	// Attempt to connect
	dial := s.dial
	if dial == nil {
		dial = func(ctx context.Context, net_, addr string) (net.Conn, error) {
			return net.Dial(net_, addr)
		}
	}
	target, err := dial(ctx, "udp", request.DestAddr.String())
	if err != nil {
		msg := err.Error()
		resp := RepHostUnreachable
		if strings.Contains(msg, "refused") {
			resp = RepConnectionRefused
		} else if strings.Contains(msg, "network is unreachable") {
			resp = RepNetworkUnreachable
		}
		if err := SendReply(writer, request.Header, resp); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("connect to %v failed, %v", request.RawDestAddr, err)
	}
	defer target.Close()

	targetUDP, ok := target.(*net.UDPConn)
	if !ok {
		if err := SendReply(writer, request.Header, RepServerFailure); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("dial udp invalid")
	}

	bindLn, err := net.ListenUDP("udp", nil)
	if err != nil {
		if err := SendReply(writer, request.Header, RepServerFailure); err != nil {
			return fmt.Errorf("failed to send reply, %v", err)
		}
		return fmt.Errorf("listen udp failed, %v", err)
	}
	defer bindLn.Close()

	s.logger.Errorf("target addr %v, listen addr: %s", targetUDP.RemoteAddr(), bindLn.LocalAddr())
	// send BND.ADDR and BND.PORT, client must
	if err = SendReply(writer, request.Header, RepSuccess, bindLn.LocalAddr()); err != nil {
		return fmt.Errorf("failed to send reply, %v", err)
	}

	s.submit(func() {
		/*
			The SOCKS UDP request/response is formed as follows:
			+-----+------+-------+----------+----------+----------+
			| RSV | FRAG |  ATYP | DST.ADDR | DST.PORT |   DATA   |
			+-----+------+-------+----------+----------+----------+
			|  2  |  1   | X'00' | Variable |     2    | Variable |
			+-----+------+-------+----------+----------+----------+
		*/
		// read from client and write to remote server
		conns := sync.Map{}
		bufPool := s.bufferPool.Get()
		defer func() {
			targetUDP.Close()
			bindLn.Close()
			s.bufferPool.Put(bufPool)
		}()
		for {
			n, srcAddr, err := bindLn.ReadFrom(bufPool[:cap(bufPool)])
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					s.logger.Errorf("read data from bind listen address %s failed, %v", bindLn.LocalAddr(), err)
					return
				}
				continue
			}

			pk := NewEmptyPacket()
			if err := pk.Parse(bufPool[:n]); err != nil {
				continue
			}

			if _, ok := conns.LoadOrStore(srcAddr.String(), struct{}{}); !ok {
				s.submit(func() {
					// read from remote server and write to client
					bufPool := s.bufferPool.Get()
					defer func() {
						targetUDP.Close()
						bindLn.Close()
						s.bufferPool.Put(bufPool)
					}()

					for {
						buf := bufPool[:cap(bufPool)]
						n, remote, err := targetUDP.ReadFrom(buf)
						if err != nil {
							s.logger.Errorf("read data from remote %s failed, %v", targetUDP.RemoteAddr(), err)
							return
						}

						pkb, err := NewPacket(remote.String(), buf[:n])
						if err != nil {
							continue
						}
						tmpBufPool := s.bufferPool.Get()
						proBuf := tmpBufPool
						proBuf = append(proBuf, pkb.Header()...)
						proBuf = append(proBuf, pkb.Data...)
						if _, err := bindLn.WriteTo(proBuf, srcAddr); err != nil {
							s.bufferPool.Put(tmpBufPool)
							s.logger.Errorf("write data to client %s failed, %v", bindLn.LocalAddr(), err)
							return
						}
						s.bufferPool.Put(tmpBufPool)
					}
				})
			}

			// 把消息写给remote sever
			if _, err := targetUDP.Write(pk.Data); err != nil {
				s.logger.Errorf("write data to remote %s failed, %v", targetUDP.RemoteAddr(), err)
				return
			}
		}
	})

	buf := s.bufferPool.Get()
	defer func() {
		s.bufferPool.Put(buf)
	}()
	for {
		_, err := request.Reader.Read(buf[:cap(buf)])
		if err != nil {
			if strings.Contains(err.Error(), "use of closed network connection") {
				return err
			}
		}
	}
}

// SendReply is used to send a reply message
func SendReply(w io.Writer, head Header, resp uint8, bindAddr ...net.Addr) error {
	/*
		The SOCKS response is formed as follows:
		+----+-----+-------+------+----------+----------+
		|VER | CMD |  RSV  | ATYP | BND.ADDR | BND.PORT |
		+----+-----+-------+------+----------+----------+
		| 1  |  1  | X'00' |  1   | Variable |    2     |
		+----+-----+-------+------+----------+----------+
	*/

	head.Command = resp

	if len(bindAddr) == 0 {
		head.addrType = ATYPIPv4
		head.Address.IP = []byte{0, 0, 0, 0}
		head.Address.Port = 0
	} else {
		addrSpec := AddrSpec{}
		if tcpAddr, ok := bindAddr[0].(*net.TCPAddr); ok && tcpAddr != nil {
			addrSpec.IP = tcpAddr.IP
			addrSpec.Port = tcpAddr.Port
		} else if udpAddr, ok := bindAddr[0].(*net.UDPAddr); ok && udpAddr != nil {
			addrSpec.IP = udpAddr.IP
			addrSpec.Port = udpAddr.Port
		} else {
			addrSpec.IP = []byte{0, 0, 0, 0}
			addrSpec.Port = 0
		}
		switch {
		case addrSpec.FQDN != "":
			head.addrType = ATYPDomain
			head.Address.FQDN = addrSpec.FQDN
			head.Address.Port = addrSpec.Port
		case addrSpec.IP.To4() != nil:
			head.addrType = ATYPIPv4
			head.Address.IP = addrSpec.IP.To4()
			head.Address.Port = addrSpec.Port
		case addrSpec.IP.To16() != nil:
			head.addrType = ATYPIPV6
			head.Address.IP = addrSpec.IP.To16()
			head.Address.Port = addrSpec.Port
		default:
			return fmt.Errorf("failed to format address[%v]", bindAddr)
		}

	}
	// Send the message
	_, err := w.Write(head.Bytes())
	return err
}

type closeWriter interface {
	CloseWrite() error
}

// Proxy is used to suffle data from src to destination, and sends errors
// down a dedicated channel
func (s *Server) Proxy(dst io.Writer, src io.Reader) error {
	buf := s.bufferPool.Get()
	defer s.bufferPool.Put(buf)
	_, err := io.CopyBuffer(dst, src, buf[:cap(buf)])
	if tcpConn, ok := dst.(closeWriter); ok {
		tcpConn.CloseWrite()
	}
	return err
}
