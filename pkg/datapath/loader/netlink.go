// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package loader

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/cilium/cilium/pkg/datapath/linux/safenetlink"
	"github.com/cilium/cilium/pkg/datapath/linux/sysctl"
	"github.com/cilium/cilium/pkg/datapath/tables"
	"github.com/cilium/cilium/pkg/datapath/tunnel"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/mac"
	mtuconst "github.com/cilium/cilium/pkg/mtu"
	"github.com/cilium/cilium/pkg/option"
)

const qdiscClsact = "clsact"

func directionToParent(dir string) uint32 {
	switch dir {
	case dirIngress:
		return netlink.HANDLE_MIN_INGRESS
	case dirEgress:
		return netlink.HANDLE_MIN_EGRESS
	}
	return 0
}

// enableForwarding puts the given link into the up state and enables IP forwarding.
func enableForwarding(logger *slog.Logger, sysctl sysctl.Sysctl, link netlink.Link) error {
	ifName := link.Attrs().Name

	if err := netlink.LinkSetUp(link); err != nil {
		logger.Warn("Could not set up the link",
			logfields.Error, err,
			logfields.Device, ifName,
		)
		return err
	}

	sysSettings := make([]tables.Sysctl, 0, 5)
	if option.Config.EnableIPv6 {
		sysSettings = append(sysSettings, tables.Sysctl{
			Name: []string{"net", "ipv6", "conf", ifName, "forwarding"}, Val: "1", IgnoreErr: false})
	}
	if option.Config.EnableIPv4 {
		sysSettings = append(sysSettings, []tables.Sysctl{
			{Name: []string{"net", "ipv4", "conf", ifName, "forwarding"}, Val: "1", IgnoreErr: false},
			{Name: []string{"net", "ipv4", "conf", ifName, "rp_filter"}, Val: "0", IgnoreErr: false},
			{Name: []string{"net", "ipv4", "conf", ifName, "accept_local"}, Val: "1", IgnoreErr: false},
			{Name: []string{"net", "ipv4", "conf", ifName, "send_redirects"}, Val: "0", IgnoreErr: false},
		}...)
	}
	if err := sysctl.ApplySettings(sysSettings); err != nil {
		return err
	}

	return nil
}

func setupVethPair(logger *slog.Logger, sysctl sysctl.Sysctl, name, peerName string) error {
	// Create the veth pair if it doesn't exist.
	if _, err := safenetlink.LinkByName(name); err != nil {
		hostMac, err := mac.GenerateRandMAC()
		if err != nil {
			return err
		}
		peerMac, err := mac.GenerateRandMAC()
		if err != nil {
			return err
		}

		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{
				Name:         name,
				HardwareAddr: net.HardwareAddr(hostMac),
				TxQLen:       1000,
			},
			PeerName:         peerName,
			PeerHardwareAddr: net.HardwareAddr(peerMac),
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return err
		}
	}

	veth, err := safenetlink.LinkByName(name)
	if err != nil {
		return err
	}
	if err := enableForwarding(logger, sysctl, veth); err != nil {
		return err
	}
	peer, err := safenetlink.LinkByName(peerName)
	if err != nil {
		return err
	}
	if err := enableForwarding(logger, sysctl, peer); err != nil {
		return err
	}

	return nil
}

// setupBaseDevice decides which and what kind of interfaces should be set up as
// the first step of datapath initialization, then performs the setup (and
// creation, if needed) of those interfaces. It returns two links and an error.
// By default, it sets up the veth pair - cilium_host and cilium_net.
func setupBaseDevice(logger *slog.Logger, sysctl sysctl.Sysctl, mtu int) (netlink.Link, netlink.Link, error) {
	if err := setupVethPair(logger, sysctl, defaults.HostDevice, defaults.SecondHostDevice); err != nil {
		return nil, nil, err
	}

	linkHost, err := safenetlink.LinkByName(defaults.HostDevice)
	if err != nil {
		return nil, nil, err
	}
	linkNet, err := safenetlink.LinkByName(defaults.SecondHostDevice)
	if err != nil {
		return nil, nil, err
	}

	if err := netlink.LinkSetARPOff(linkHost); err != nil {
		return nil, nil, err
	}
	if err := netlink.LinkSetARPOff(linkNet); err != nil {
		return nil, nil, err
	}

	if err := netlink.LinkSetMTU(linkHost, mtu); err != nil {
		return nil, nil, err
	}
	if err := netlink.LinkSetMTU(linkNet, mtu); err != nil {
		return nil, nil, err
	}

	return linkHost, linkNet, nil
}

// addHostDeviceAddr add internal ipv4 and ipv6 addresses to the cilium_host device.
func addHostDeviceAddr(hostDev netlink.Link, ipv4, ipv6 net.IP) error {
	if ipv4 != nil {
		addr := netlink.Addr{
			IPNet: &net.IPNet{
				IP:   ipv4,
				Mask: net.CIDRMask(32, 32), // corresponds to /32
			},
		}

		if err := netlink.AddrReplace(hostDev, &addr); err != nil {
			return err
		}
	}
	if ipv6 != nil {
		addr := netlink.Addr{
			IPNet: &net.IPNet{
				IP:   ipv6,
				Mask: net.CIDRMask(128, 128), // corresponds to /128
			},
		}

		if err := netlink.AddrReplace(hostDev, &addr); err != nil {
			return err
		}
	}
	return nil
}

// setupTunnelDevice ensures the cilium_{mode} device is created and
// unused leftover devices are cleaned up in case mode changes.
func setupTunnelDevice(logger *slog.Logger, sysctl sysctl.Sysctl, mode tunnel.EncapProtocol, port, srcPortLow, srcPortHigh uint16, mtu int) error {
	switch mode {
	case tunnel.Geneve:
		if err := setupGeneveDevice(logger, sysctl, port, srcPortLow, srcPortHigh, mtu); err != nil {
			return fmt.Errorf("setting up geneve device: %w", err)
		}
		if err := removeDevice(defaults.VxlanDevice); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.VxlanDevice, err)
		}

	case tunnel.VXLAN:
		if err := setupVxlanDevice(logger, sysctl, port, srcPortLow, srcPortHigh, mtu); err != nil {
			return fmt.Errorf("setting up vxlan device: %w", err)
		}
		if err := removeDevice(defaults.GeneveDevice); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.GeneveDevice, err)
		}

	default:
		if err := removeDevice(defaults.VxlanDevice); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.VxlanDevice, err)
		}
		if err := removeDevice(defaults.GeneveDevice); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.GeneveDevice, err)
		}
	}

	return nil
}

// setupGeneveDevice ensures the cilium_geneve device is created with the given
// destination port and mtu.
//
// Changing the destination port will recreate the device. Changing the MTU will
// modify the device without recreating it.
func setupGeneveDevice(logger *slog.Logger, sysctl sysctl.Sysctl, dport, srcPortLow, srcPortHigh uint16, mtu int) error {
	mac, err := mac.GenerateRandMAC()
	if err != nil {
		return err
	}
	// In geneve driver kernel defaults to [1,USHRT_MAX]
	if srcPortLow == 0 && srcPortHigh == 0 {
		srcPortLow = 1
		srcPortHigh = math.MaxUint16
	}

	dev := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name:         defaults.GeneveDevice,
			MTU:          mtu,
			HardwareAddr: net.HardwareAddr(mac),
		},
		FlowBased: true,
		Dport:     dport,
		PortLow:   int(srcPortLow),
		PortHigh:  int(srcPortHigh),
	}

	l, err := ensureDevice(logger, sysctl, dev)
	if err != nil {
		return fmt.Errorf("creating geneve device: %w", err)
	}

	// Recreate the device with the correct destination port. Modifying the device
	// without recreating it is not supported.
	geneve, _ := l.(*netlink.Geneve)
	if geneve.Dport != dport {
		if err := netlink.LinkDel(l); err != nil {
			return fmt.Errorf("deleting outdated geneve device: %w", err)
		}
		if _, err := ensureDevice(logger, sysctl, dev); err != nil {
			return fmt.Errorf("recreating geneve device %s: %w", defaults.GeneveDevice, err)
		}
	}
	if geneve.PortLow != int(srcPortLow) || geneve.PortHigh != int(srcPortHigh) {
		logger.Info(
			"Source port range hint ignored given geneve device already exists",
			logfields.Hint, fmt.Sprintf("(%d-%d)", int(srcPortLow), int(srcPortHigh)),
			logfields.Range, fmt.Sprintf("(%d-%d)", geneve.PortLow, geneve.PortHigh),
			logfields.Device, defaults.GeneveDevice,
		)
	}
	return nil
}

// setupVxlanDevice ensures the cilium_vxlan device is created with the given
// port, source port range, and MTU.
//
// Changing the port will recreate the device. Changing the MTU will modify the
// device without recreating it. Changing the source port range at runtime is
// not possible, and it's also not worth to recreate. It's a best effort hint
// for first-time creation.
func setupVxlanDevice(logger *slog.Logger, sysctl sysctl.Sysctl, port, srcPortLow, srcPortHigh uint16, mtu int) error {
	mac, err := mac.GenerateRandMAC()
	if err != nil {
		return err
	}

	dev := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:         defaults.VxlanDevice,
			MTU:          mtu,
			HardwareAddr: net.HardwareAddr(mac),
		},
		FlowBased: true,
		Port:      int(port),
		PortLow:   int(srcPortLow),
		PortHigh:  int(srcPortHigh),
	}

	// It's possible to create multiple vxlan devices with the same dstport,
	// though only one of them can be 'up'. Delete an existing vxlan device
	// with a mismatching port before attempting to create a new one to
	// avoid ensureDevice setting up the old interface. This avoids the
	// agent getting stuck if it conflicts with an unmanaged vxlan interface.
	if l, err := safenetlink.LinkByName(dev.Attrs().Name); err == nil {
		// Recreate the device with the correct destination port. Modifying the device
		// without recreating it is not supported.
		vxlan, _ := l.(*netlink.Vxlan)
		if vxlan.Port != int(port) {
			if err := netlink.LinkDel(l); err != nil {
				return fmt.Errorf("deleting outdated vxlan device: %w", err)
			}
		}
	}

	l, err := ensureDevice(logger, sysctl, dev)
	if err != nil {
		return fmt.Errorf("creating vxlan device %s: %w", dev.Attrs().Name, err)
	}

	vxlan, _ := l.(*netlink.Vxlan)
	if vxlan.PortLow != int(srcPortLow) || vxlan.PortHigh != int(srcPortHigh) {
		logger.Info(
			"Source port range hint ignored given vxlan device already exists",
			logfields.Hint, fmt.Sprintf("(%d-%d)", int(srcPortLow), int(srcPortHigh)),
			logfields.Range, fmt.Sprintf("(%d-%d)", vxlan.PortLow, vxlan.PortHigh),
			logfields.Device, defaults.VxlanDevice,
		)
	}
	return nil
}

// setupIPIPDevices ensures the specified v4 and/or v6 devices are created and
// configured with their respective sysctls.
//
// Calling this function may result in tunl0 (v4) or ip6tnl0 (v6) fallback
// interfaces being created as a result of loading the ipip and ip6_tunnel
// kernel modules by creating cilium_ tunnel interfaces. These are catch-all
// interfaces for the ipip decapsulation stack. By default, these interfaces
// will be created in new network namespaces, but Cilium disables this behaviour
// by setting net.core.fb_tunnels_only_for_init_net = 2.
//
// In versions of Cilium prior to 1.15, the behaviour was as follows:
//   - Repurpose the default tunl0 by setting it into collect_md mode and renaming
//     it to cilium_ipip4. Use the interface for production traffic.
//   - The same cannot be done for ip6tunl0, as collect_md cannot be enabled on
//     this interface. Leave it unused.
//   - Rename sit0 to cilium_sit, if present. This was potentially a mistake,
//     as the sit module is not involved with ip6tnl interfaces.
//
// As of Cilium 1.15, if present, tunl0 is renamed to cilium_tunl and ip6tnl0 is
// renamed to cilium_ip6tnl. This is to communicate to the user that Cilium has
// taken control of the encapsulation stack on the node, as it currently doesn't
// explicitly support sharing it with other tools/CNIs. Fallback devices are left
// unused for production traffic. Only devices that were explicitly created are
// used. As of Cilium 1.18, cilium_tunl and cilium_ip6tnl are not created anymore.
func setupIPIPDevices(logger *slog.Logger, sysctl sysctl.Sysctl, ipv4, ipv6 bool, mtu int) error {
	// FlowBased sets IFLA_IPTUN_COLLECT_METADATA, the equivalent of 'ip link add
	// ... type ipip/ip6tnl external'. This is needed so bpf programs can use
	// bpf_skb_[gs]et_tunnel_key() on packets flowing through tunnels.
	if ipv4 {
		dev := &netlink.Iptun{
			LinkAttrs: netlink.LinkAttrs{
				Name: defaults.IPIPv4Device,
				MTU:  mtu - mtuconst.IPIPv4Overhead,
			},
			FlowBased: true,
		}

		if _, err := ensureDevice(logger, sysctl, dev); err != nil {
			return fmt.Errorf("creating %s: %w", defaults.IPIPv4Device, err)
		}

		// Rename fallback device created by potential kernel module load after
		// creating tunnel interface.
		if err := renameDevice("tunl0", "cilium_tunl"); err != nil {
			return fmt.Errorf("renaming fallback device %s: %w", "tunl0", err)
		}
	} else {
		if err := removeDevice(defaults.IPIPv4Device); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.IPIPv4Device, err)
		}
	}

	if ipv6 {
		dev := &netlink.Ip6tnl{
			LinkAttrs: netlink.LinkAttrs{
				Name: defaults.IPIPv6Device,
				MTU:  mtu - mtuconst.IPIPv6Overhead,
			},
			FlowBased: true,
		}

		if _, err := ensureDevice(logger, sysctl, dev); err != nil {
			return fmt.Errorf("creating %s: %w", defaults.IPIPv6Device, err)
		}

		// Rename fallback device created by potential kernel module load after
		// creating tunnel interface.
		if err := renameDevice("ip6tnl0", "cilium_ip6tnl"); err != nil {
			return fmt.Errorf("renaming fallback device %s: %w", "tunl0", err)
		}
	} else {
		if err := removeDevice(defaults.IPIPv6Device); err != nil {
			return fmt.Errorf("removing %s: %w", defaults.IPIPv6Device, err)
		}
	}

	return nil
}

// ensureDevice ensures a device with the given attrs is present on the system.
// If a device with the given name already exists, device creation is skipped and
// the existing device will be used as-is for the subsequent configuration steps.
// The device is never recreated.
//
// The device's state is set to 'up', L3 forwarding sysctls are applied, and MTU
// is set.
func ensureDevice(logger *slog.Logger, sysctl sysctl.Sysctl, attrs netlink.Link) (netlink.Link, error) {
	name := attrs.Attrs().Name

	// Reuse existing tunnel interface created by previous runs.
	l, err := safenetlink.LinkByName(name)
	if err != nil {
		if err := netlink.LinkAdd(attrs); err != nil {
			if errors.Is(err, unix.ENOTSUP) {
				err = fmt.Errorf("%w, maybe kernel module for %s is not available?", err, attrs.Type())
			}
			return nil, fmt.Errorf("creating device %s: %w", name, err)
		}

		// Fetch the link we've just created.
		l, err = safenetlink.LinkByName(name)
		if err != nil {
			return nil, fmt.Errorf("retrieving created device %s: %w", name, err)
		}
	}

	if err := enableForwarding(logger, sysctl, l); err != nil {
		return nil, fmt.Errorf("setting up device %s: %w", name, err)
	}

	// Update MTU on the link if necessary.
	wantMTU, gotMTU := attrs.Attrs().MTU, l.Attrs().MTU
	if wantMTU != 0 && wantMTU != gotMTU {
		if err := netlink.LinkSetMTU(l, wantMTU); err != nil {
			return nil, fmt.Errorf("setting MTU on %s: %w", name, err)
		}
	}

	return l, nil
}

// removeDevice removes the device with the given name. Returns error if the
// device exists but was unable to be removed.
func removeDevice(name string) error {
	link, err := safenetlink.LinkByName(name)
	if err != nil {
		return nil
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("removing device %s: %w", name, err)
	}

	return nil
}

// renameDevice renames a network device from and to a given value. Returns nil
// if the device does not exist.
func renameDevice(from, to string) error {
	link, err := safenetlink.LinkByName(from)
	if err != nil {
		return nil
	}

	if err := netlink.LinkSetName(link, to); err != nil {
		return fmt.Errorf("renaming device %s to %s: %w", from, to, err)
	}

	return nil
}

// DeviceHasSKBProgramLoaded returns true if the given device has a tc(x) program
// attached.
//
// If checkEgress is true, returns true if there's both an ingress and
// egress program attached.
func DeviceHasSKBProgramLoaded(device string, checkEgress bool) (bool, error) {
	link, err := safenetlink.LinkByName(device)
	if err != nil {
		return false, fmt.Errorf("retrieving device %s: %w", device, err)
	}

	itcx, err := hasCiliumTCXLinks(link, ebpf.AttachTCXIngress)
	if err != nil {
		return false, err
	}
	itc, err := hasCiliumTCFilters(link, netlink.HANDLE_MIN_INGRESS)
	if err != nil {
		return false, err
	}
	ink, err := hasCiliumNetkitLinks(link, ebpf.AttachNetkitPeer)
	if err != nil {
		return false, err
	}

	// Need ingress programs at minimum, bail out if these are already missing.
	if !itc && !itcx && !ink {
		return false, nil
	}

	if !checkEgress {
		return true, nil
	}

	etcx, err := hasCiliumTCXLinks(link, ebpf.AttachTCXEgress)
	if err != nil {
		return false, err
	}
	etc, err := hasCiliumTCFilters(link, netlink.HANDLE_MIN_EGRESS)
	if err != nil {
		return false, err
	}
	enk, err := hasCiliumNetkitLinks(link, ebpf.AttachNetkitPrimary)
	if err != nil {
		return false, err
	}

	return etc || etcx || enk, nil
}
