package nats

import (
	"fmt"
	"log"
	"net"

	"github.com/pion/logging"
	"github.com/pion/stun"
	"github.com/pion/turn"
)

// EndpointDependencyType ...
type EndpointDependencyType uint8

const (
	// EndpointIndependent means the behavior is independent of the endpoint's address or port
	EndpointIndependent EndpointDependencyType = iota
	// EndpointAddrDependent means the behavior is dependent on the endpoint's address
	EndpointAddrDependent
	// EndpointAddrPortDependent means the behavior is dependent on the endpoint's address and port
	EndpointAddrPortDependent
	// EndpointUndefined ...
	EndpointUndefined
)

func (t EndpointDependencyType) String() string {
	switch t {
	case EndpointIndependent:
		return "independent"
	case EndpointAddrDependent:
		return "address dependent"
	case EndpointAddrPortDependent:
		return "address-port dependent"
	}
	return "unspecified"
}

// DiscoverResult contains a set of results from Discover method.
type DiscoverResult struct {
	IsNatted          bool                   `json:"isNatted"`
	MappingBehavior   EndpointDependencyType `json:"mappingBehavior"`
	FilteringBehavior EndpointDependencyType `json:"filteringBehavior"`
	PortPreservation  bool                   `json:"portPreservation"`
	NATType           string                 `json:"natType"`
	ExternalIP        string                 `json:"externalIP"`
}

// Discover performs NAT discovery process defined in RFC 5780.
func Discover(server string, verbose bool) (*DiscoverResult, error) {
	server = formatHostPort(server, 3478)

	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	locAddr := conn.LocalAddr().(*net.UDPAddr)
	if verbose {
		log.Printf("Local port: %d", locAddr.Port)
	}

	c, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: server,
		Conn:           conn,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return nil, err
	}

	err = c.Listen()
	if err != nil {
		return nil, err
	}

	if verbose {
		log.Printf("STUN server: %s", c.STUNServerAddr().String())
	}

	toAddrs := [4]*net.UDPAddr{c.STUNServerAddr().(*net.UDPAddr), nil, nil, nil}
	mappedAddrs := [4]*net.UDPAddr{nil, nil, nil, nil}

	res := &DiscoverResult{}

	// Run filtering behavior disocvery in parallel
	filterDiscovDone, err := discoverFilteringBehavior(server)

	// Mapping behavior desicovery

	for i := 0; i < len(toAddrs); i++ {
		to := toAddrs[i]
		attrs := []stun.Setter{
			stun.TransactionID,
			stun.BindingRequest,
		}

		msg, err := stun.Build(attrs...)
		if err != nil {
			return nil, err
		}

		trRes, err := c.PerformTransaction(msg, to, false)
		if err != nil {
			return nil, err
		}

		var maddr stun.XORMappedAddress
		if err = maddr.GetFrom(trRes.Msg); err != nil {
			if err != nil {
				return nil, fmt.Errorf("XOR-MAPPED-ADDRESS not found")
			}
		}
		mappedAddrs[i] = &net.UDPAddr{IP: maddr.IP, Port: maddr.Port}

		if verbose {
			log.Printf("MAPPED-ADDRESS [%d]: %s", i, mappedAddrs[i].String())
		}

		if i == 0 {
			res.IsNatted = !findIsLocalIP(mappedAddrs[0].IP)
			res.PortPreservation = (mappedAddrs[0].Port == locAddr.Port)
			res.ExternalIP = mappedAddrs[0].IP.String()

			var caddr attrAddress
			if err = caddr.getAs(trRes.Msg, attrTypeChangedAddress); err != nil {
				if err != nil {
					return nil, fmt.Errorf("CHANGED-ADDRESS not found")
				}
			}

			if verbose {
				log.Printf("CHANGED-ADDRESS: %s", caddr.String())
			}

			toAddrs[1] = &net.UDPAddr{IP: toAddrs[0].IP, Port: caddr.Port}
			toAddrs[2] = &net.UDPAddr{IP: caddr.IP, Port: toAddrs[0].Port}
			toAddrs[3] = &net.UDPAddr{IP: caddr.IP, Port: caddr.Port}

			continue
		}
	}

	if res.IsNatted {
		if mappedAddrs[0].Port != mappedAddrs[2].Port {
			if mappedAddrs[0].Port != mappedAddrs[1].Port {
				res.MappingBehavior = EndpointAddrPortDependent
			} else {
				res.MappingBehavior = EndpointAddrDependent
			}
		}
	}

	// Wait for filtering behavior disocvery to complete
	res.FilteringBehavior = <-filterDiscovDone

	// Determine the NAT type
	if res.IsNatted {
		if res.MappingBehavior == EndpointIndependent {
			switch res.FilteringBehavior {
			case EndpointIndependent:
				res.NATType = "Full cone NAT"
			case EndpointAddrDependent:
				res.NATType = "Address-restricted cone NAT"
			case EndpointAddrPortDependent:
				res.NATType = "Port-restricted cone NAT"
			default:
				res.NATType = "(undefined)"
			}
		} else {
			res.NATType = "Symmetric NAT"
		}
	} else {
		if res.FilteringBehavior == EndpointIndependent {
			res.NATType = "Open to the Internet"
		} else {
			res.NATType = "UDP blocked by firewall"
		}
	}

	return res, nil
}

// Test if this IP is a local IP.
func findIsLocalIP(ip net.IP) bool {
	// If we can bind this IP, it is a valid local IP address.
	conn, err := net.ListenPacket("udp", fmt.Sprintf("%s:0", ip.String))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func discoverFilteringBehavior(server string) (<-chan EndpointDependencyType, error) {
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}

	c, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: server,
		Conn:           conn,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return nil, err
	}

	err = c.Listen()
	if err != nil {
		return nil, err
	}

	done := make(chan EndpointDependencyType)

	go func() {
		defer c.Close()
		defer conn.Close()

		received1Ch, err2 := performTransactionWith(c, true, false)
		if err2 != nil {
			done <- EndpointUndefined
			return
		}
		received2Ch, err2 := performTransactionWith(c, false, true)
		if err2 != nil {
			done <- EndpointUndefined
			return
		}

		received1 := <-received1Ch
		received2 := <-received2Ch

		if received1 {
			if received2 {
				done <- EndpointIndependent
			} else {
				done <- EndpointAddrDependent
			}
		} else {
			done <- EndpointAddrPortDependent
		}
	}()

	return done, nil
}

func performTransactionWith(c *turn.Client, changeIP, changePort bool) (<-chan bool, error) {
	attrs := []stun.Setter{
		stun.TransactionID,
		stun.BindingRequest,
	}

	msg, err := stun.Build(attrs...)
	if err != nil {
		return nil, err
	}

	err = (&attrChangeRequest{
		ChangeIP: true,
	}).addAs(msg, attrTypeChangeRequest)
	if err != nil {
		return nil, err
	}

	receivedCh := make(chan bool)

	go func() {
		_, err = c.PerformTransaction(msg, c.STUNServerAddr(), false)
		if err != nil {
			receivedCh <- false
			return
		}
		receivedCh <- true
	}()

	return receivedCh, nil
}

// Appends default port number if the given host name does not have it.
func formatHostPort(host string, defaultPort int) string {
	_, _, err := net.SplitHostPort(host)
	if err != nil {
		return fmt.Sprintf("%s:%d", host, defaultPort)
	}
	return host
}