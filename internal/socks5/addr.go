package socks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
)

// Addr represents a SOCKS5 address (IPv4, IPv6, or domain name)
type Addr struct {
	Type   byte
	IP     net.IP
	Domain string
	Port   uint16
}

func (a *Addr) Network() string { return "tcp" }

func (a *Addr) String() string {
	switch a.Type {
	case AtypIPv4, AtypIPv6:
		return net.JoinHostPort(a.IP.String(), strconv.Itoa(int(a.Port)))
	case AtypDomain:
		return net.JoinHostPort(a.Domain, strconv.Itoa(int(a.Port)))
	}
	return ""
}

// ReadAddr reads a SOCKS5 address from the reader
func ReadAddr(r io.Reader) (*Addr, error) {
	addr := &Addr{}

	// Read address type
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(r, atyp); err != nil {
		return nil, fmt.Errorf("read atyp: %w", err)
	}
	addr.Type = atyp[0]

	// Read address based on type
	switch addr.Type {
	case AtypIPv4:
		addr.IP = make(net.IP, 4)
		if _, err := io.ReadFull(r, addr.IP); err != nil {
			return nil, fmt.Errorf("read IPv4: %w", err)
		}
	case AtypIPv6:
		addr.IP = make(net.IP, 16)
		if _, err := io.ReadFull(r, addr.IP); err != nil {
			return nil, fmt.Errorf("read IPv6: %w", err)
		}
	case AtypDomain:
		// Domain: first byte is length
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return nil, fmt.Errorf("read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(r, domain); err != nil {
			return nil, fmt.Errorf("read domain: %w", err)
		}
		addr.Domain = string(domain)
	default:
		return nil, fmt.Errorf("unsupported address type: %d", addr.Type)
	}

	// Read port (2 bytes, big-endian)
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return nil, fmt.Errorf("read port: %w", err)
	}
	addr.Port = binary.BigEndian.Uint16(portBuf)

	return addr, nil
}

// WriteTo writes the SOCKS5 address to the writer
func (a *Addr) WriteTo(w io.Writer) (int64, error) {
	var buf []byte

	switch a.Type {
	case AtypIPv4:
		buf = make([]byte, 1+4+2)
		buf[0] = AtypIPv4
		copy(buf[1:5], a.IP.To4())
		binary.BigEndian.PutUint16(buf[5:], a.Port)
	case AtypIPv6:
		buf = make([]byte, 1+16+2)
		buf[0] = AtypIPv6
		copy(buf[1:17], a.IP.To16())
		binary.BigEndian.PutUint16(buf[17:], a.Port)
	case AtypDomain:
		buf = make([]byte, 1+1+len(a.Domain)+2)
		buf[0] = AtypDomain
		buf[1] = byte(len(a.Domain))
		copy(buf[2:], a.Domain)
		binary.BigEndian.PutUint16(buf[2+len(a.Domain):], a.Port)
	default:
		return 0, fmt.Errorf("unsupported address type: %d", a.Type)
	}

	n, err := w.Write(buf)
	return int64(n), err
}

// AddrFromNetAddr creates an Addr from a net.Addr
func AddrFromNetAddr(addr net.Addr) *Addr {
	switch a := addr.(type) {
	case *net.TCPAddr:
		result := &Addr{Port: uint16(a.Port)}
		if ip4 := a.IP.To4(); ip4 != nil {
			result.Type = AtypIPv4
			result.IP = ip4
		} else {
			result.Type = AtypIPv6
			result.IP = a.IP.To16()
		}
		return result
	default:
		return &Addr{Type: AtypIPv4, IP: net.IPv4zero, Port: 0}
	}
}
