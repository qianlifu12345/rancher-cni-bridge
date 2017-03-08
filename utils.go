package main

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/ip"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/vishvananda/netlink"
)

func ensureBridgeAddr(br *netlink.Bridge, ipn *net.IPNet) error {
	addrs, err := netlink.AddrList(br, syscall.AF_INET)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not  list of IP addresses: %v", err)
	}

	// if there're no addresses on the bridge, it's ok -- we'll add one
	if len(addrs) > 0 {
		ipnStr := ipn.String()
		for _, a := range addrs {
			// string comp is actually easiest for doing IPNet comps
			if a.IPNet.String() == ipnStr {
				return nil
			}
		}
		return fmt.Errorf("%q already has an IP address different from %v", br.Name, ipn.String())
	}

	addr := &netlink.Addr{IPNet: ipn, Label: ""}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return fmt.Errorf("could not add IP address to %q: %v", br.Name, err)
	}
	return nil
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func ensureBridge(brName string, mtu int) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}

	if err := netlink.LinkAdd(br); err != nil {
		if err != syscall.EEXIST {
			return nil, fmt.Errorf("could not add %q: %v", brName, err)
		}

		// it's ok if the device already exists as long as config is similar
		br, err = bridgeByName(brName)
		if err != nil {
			return nil, err
		}
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	return br, nil
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int, hairpinMode bool) error {
	var hostVethName string

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, _, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}

		hostVethName = hostVeth.Attrs().Name
		return nil
	})
	if err != nil {
		return err
	}

	// need to lookup hostVeth again as its index has changed during ns move
	hostVeth, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", hostVethName, err)
	}

	// connect host veth end to the bridge
	if err = netlink.LinkSetMaster(hostVeth, br); err != nil {
		return fmt.Errorf("failed to connect %q to bridge %v: %v", hostVethName, br.Attrs().Name, err)
	}

	// set hairpin mode
	if err = netlink.LinkSetHairpin(hostVeth, hairpinMode); err != nil {
		return fmt.Errorf("failed to setup hairpin mode for %v: %v", hostVethName, err)
	}

	return nil
}

func calcGatewayIP(ipn *net.IPNet) net.IP {
	nid := ipn.IP.Mask(ipn.Mask)
	return ip.NextIP(nid)
}
func calculateBridgeIP(n *NetConf) (*net.IPNet, error) {
	var (
		ip          net.IP
		bridgeIPNet *net.IPNet
		err         error
	)

	if n.BrSubnet == "" {
		return nil, fmt.Errorf("mandatory bridgeSubnet not specified in config")
	}

	_, brNetworkIPNet, err := net.ParseCIDR(n.BrSubnet)
	if err != nil {
		return nil, fmt.Errorf("Invalid bridgeSubnet specified got error: %v", err)
	}

	if n.BrIP != "" {
		ip = net.ParseIP(n.BrIP)
		if ip == nil {
			// Check if we can parse as a CIDR
			ip, _, err = net.ParseCIDR(n.BrIP)
			if err != nil {
				return nil, fmt.Errorf("invalid bridgeIP specified in config")
			}
		}

		if !brNetworkIPNet.Contains(ip) {
			return nil, fmt.Errorf("bridgeIP is not in bridgeSubnet")
		}
		bridgeIPNet = &net.IPNet{IP: ip, Mask: brNetworkIPNet.Mask}
	} else {
		// Use the first IP of the subnet for the bridge
		brNetworkIPTo4 := brNetworkIPNet.IP.To4()

		ip = net.IPv4(
			brNetworkIPTo4[0],
			brNetworkIPTo4[1],
			brNetworkIPTo4[2],
			brNetworkIPTo4[3]+1,
		)
		bridgeIPNet = &net.IPNet{IP: ip, Mask: brNetworkIPNet.Mask}
	}

	return bridgeIPNet, nil
}

func setBridgeIP(n *NetConf) error {

	if n.BrSubnet == "" {
		return fmt.Errorf("mandatory bridgeSubnet not specified in config")
	}

	link, err := netlink.LinkByName(n.BrName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", n.BrName, err)
	}

	bridgeIPNet, err := calculateBridgeIP(n)
	if err != nil {
		return fmt.Errorf("failed to calculate bridge IP: %v", err)
	}

	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not get list of IP addresses: %v", err)
	}
	if len(addrs) > 0 {
		bridgeIPStr := bridgeIPNet.String()
		for _, a := range addrs {
			if a.IPNet.String() == bridgeIPStr {
				// Bridge IP already set, nothing to do
				return nil
			}
		}
	}

	addr := &netlink.Addr{IPNet: bridgeIPNet, Label: ""}
	if err = netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add IP addr to %q: %v", n.BrName, err)
	}

	return nil
}

func setupBridge(n *NetConf) (*netlink.Bridge, error) {
	// create bridge if necessary
	br, err := ensureBridge(n.BrName, n.MTU)
	if err != nil {
		return nil, fmt.Errorf("failed to create bridge %q: %v", n.BrName, err)
	}

	// Set the bridge IP address
	//err = setBridgeIP(n)
	//if err != nil {
	//	return nil, fmt.Errorf("failed to set bridge IP: %v", err)
	//}

	return br, nil
}

// configureInterface takes the result of IPAM plugin and
// applies to the ifName interface
func configureInterface(ifName string, res *types.Result) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", ifName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("failed to set %q UP: %v", ifName, err)
	}

	// TODO(eyakubovich): IPv6
	addr := &netlink.Addr{IPNet: &res.IP4.IP, Label: ""}
	if err = netlink.AddrAdd(link, addr); err != nil {
		if err.Error() == "file exists" {
			logrus.Infof("rancher-cni-bridge: Interface %q already has IP address: %v, no worries", ifName, addr)
		} else {
			return fmt.Errorf("failed to add IP addr to %q: %v", ifName, err)
		}
	}

	for _, r := range res.IP4.Routes {
		gw := r.GW
		if gw == nil {
			gw = res.IP4.Gateway
		}
		if err = ip.AddRoute(&r.Dst, gw, link); err != nil {
			// we skip over duplicate routes as we assume the first one wins
			if !os.IsExist(err) {
				return fmt.Errorf("failed to add route '%v via %v dev %v': %v", r.Dst, gw, ifName, err)
			}
		}
	}

	return nil
}

func checkIfContainerInterfaceExists(args *skel.CmdArgs) bool {
	err := ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		_, err := netlink.LinkByName(args.IfName)
		if err != nil {
			return fmt.Errorf("failed to lookup %q: %v", args.IfName, err)
		}
		return nil
	})

	if err == nil {
		return true
	}
	return false
}

func setInterfaceMacAddress(ifName, mac string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", ifName, err)
	}

	hwaddr, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("failed to parse MAC address: %v", err)
	}
	err = netlink.LinkSetHardwareAddr(link, hwaddr)
	if err != nil {
		return fmt.Errorf("failed to set hw address of interface: %v", err)
	}

	return nil
}
