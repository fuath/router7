// Package dhcp4 implements a DHCPv4 client.
package dhcp4

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/d2g/dhcp4"
	"github.com/d2g/dhcp4client"
)

type Config struct {
	RenewAfter time.Time `json:"valid_until"`
	ClientIP   string    `json:"client_ip"`   // e.g. 85.195.207.62
	SubnetMask string    `json:"subnet_mask"` // e.g. 255.255.255.128
	Router     string    `json:"router"`      // e.g. 85.195.207.1
	DNS        []string  `json:"dns"`         // e.g. 77.109.128.2, 213.144.129.20
}

type Client struct {
	Interface *net.Interface // e.g. net.InterfaceByName("eth0")

	err          error
	once         sync.Once
	dhcp         *dhcp4client.Client
	connection   dhcp4client.ConnectionInt
	hardwareAddr net.HardwareAddr
	cfg          Config
	timeNow      func() time.Time
	randRead     func([]byte) (int, error)
}

// ObtainOrRenew returns false when encountering a permanent error.
func (c *Client) ObtainOrRenew() bool {
	c.once.Do(func() {
		if c.timeNow == nil {
			c.timeNow = time.Now
		}
		if c.randRead == nil {
			c.randRead = rand.Read
		}
		if c.connection == nil && c.Interface != nil {
			pktsock, err := dhcp4client.NewPacketSock(c.Interface.Index)
			if err != nil {
				c.err = err
				return
			}
			c.connection = pktsock
		}
		if c.connection == nil && c.Interface == nil {
			c.err = fmt.Errorf("Interface is nil")
			return
		}
		if c.hardwareAddr == nil {
			c.hardwareAddr = c.Interface.HardwareAddr
		}
		dhcp, err := dhcp4client.New(
			dhcp4client.HardwareAddr(c.hardwareAddr),
			dhcp4client.Timeout(5*time.Second),
			dhcp4client.Broadcast(false),
			dhcp4client.Connection(c.connection),
		)
		if err != nil {
			c.err = err
			return
		}
		dhcp.RandRead = c.randRead
		c.dhcp = dhcp
	})
	if c.err != nil {
		return false
	}
	ok, ack, err := c.dhcpRequest()
	if err != nil {
		c.err = err
		return true // temporary error
	}
	if !ok {
		c.err = fmt.Errorf("received DHCPNAK")
		return true // temporary error
	}
	opts := ack.ParseOptions()

	// DHCPACK (described in RFC2131 4.3.1)
	// - yiaddr: IP address assigned to client
	c.cfg.ClientIP = ack.YIAddr().String()

	if b, ok := opts[dhcp4.OptionSubnetMask]; ok {
		mask := net.IPMask(b)
		c.cfg.SubnetMask = fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
	}

	// if b, ok := opts[dhcp4.OptionBroadcastAddress]; ok {
	// 	if err := cs.SetBroadcast(net.IP(b)); err != nil {
	// 		log.Fatalf("setBroadcast(%v): %v", net.IP(b), err)
	// 	}
	// }

	if b, ok := opts[dhcp4.OptionRouter]; ok {
		c.cfg.Router = net.IP(b).String()
	}

	if b, ok := opts[dhcp4.OptionDomainNameServer]; ok {
		c.cfg.DNS = nil
		for len(b) > 0 {
			c.cfg.DNS = append(c.cfg.DNS, net.IP(b[:4]).String())
			b = b[4:]
		}
	}

	leaseTime := 10 * time.Minute // seems sensible as a fallback
	if b, ok := opts[dhcp4.OptionIPAddressLeaseTime]; ok && len(b) == 4 {
		leaseTime = parseDHCPDuration(b)
	}

	// As per RFC 2131 section 4.4.5:
	// renewal time defaults to 50% of the lease time
	renewalTime := time.Duration(float64(leaseTime) * 0.5)
	if b, ok := opts[dhcp4.OptionRenewalTimeValue]; ok && len(b) == 4 {
		renewalTime = parseDHCPDuration(b)
	}
	c.cfg.RenewAfter = c.timeNow().Add(renewalTime)
	return true
}

func (c *Client) Err() error {
	return c.err
}

func (c *Client) Config() Config {
	return c.cfg
}

func parseDHCPDuration(b []byte) time.Duration {
	return time.Duration(binary.BigEndian.Uint32(b)) * time.Second
}

func (c *Client) addHostname(p *dhcp4.Packet) {
	var utsname unix.Utsname
	if err := unix.Uname(&utsname); err != nil {
		log.Fatal(err)
	}
	nnb := utsname.Nodename[:bytes.IndexByte(utsname.Nodename[:], 0)]
	p.AddOption(dhcp4.OptionHostName, nnb)
}

func (c *Client) addClientId(p *dhcp4.Packet) {
	id := make([]byte, len(c.hardwareAddr)+1)
	id[0] = 1 // hardware type ethernet, https://tools.ietf.org/html/rfc1700
	copy(id[1:], c.hardwareAddr)
	p.AddOption(dhcp4.OptionClientIdentifier, id)
}

// dhcpRequest is a copy of (dhcp4client/Client).Request which
// includes the hostname.
func (c *Client) dhcpRequest() (bool, dhcp4.Packet, error) {
	discoveryPacket := c.dhcp.DiscoverPacket()
	c.addHostname(&discoveryPacket)
	c.addClientId(&discoveryPacket)
	discoveryPacket.PadToMinSize()

	if err := c.dhcp.SendPacket(discoveryPacket); err != nil {
		return false, discoveryPacket, err
	}

	offerPacket, err := c.dhcp.GetOffer(&discoveryPacket)
	if err != nil {
		return false, offerPacket, err
	}

	requestPacket := c.dhcp.RequestPacket(&offerPacket)
	c.addHostname(&requestPacket)
	c.addClientId(&requestPacket)
	requestPacket.PadToMinSize()

	if err := c.dhcp.SendPacket(requestPacket); err != nil {
		return false, requestPacket, err
	}

	acknowledgement, err := c.dhcp.GetAcknowledgement(&requestPacket)
	if err != nil {
		return false, acknowledgement, err
	}

	acknowledgementOptions := acknowledgement.ParseOptions()
	if dhcp4.MessageType(acknowledgementOptions[dhcp4.OptionDHCPMessageType][0]) != dhcp4.ACK {
		return false, acknowledgement, nil
	}

	return true, acknowledgement, nil
}
