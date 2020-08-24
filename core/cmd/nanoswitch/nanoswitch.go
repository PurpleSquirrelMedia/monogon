// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// nanoswitch is a virtualized switch/router combo intended for testing.
// It uses the first interface as an external interface to connect to the host and pass traffic in and out. All other
// interfaces are switched together and served by a built-in DHCP server. Traffic from that network to the
// SLIRP/external network is SNATed as the host-side SLIRP ignores routed packets.
// It also has built-in userspace proxying support for debugging.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"git.monogon.dev/source/nexantic.git/core/internal/common"
	"git.monogon.dev/source/nexantic.git/core/internal/common/supervisor"
	"git.monogon.dev/source/nexantic.git/core/internal/launch"
	"git.monogon.dev/source/nexantic.git/core/internal/network/dhcp"
)

var switchIP = net.IP{10, 1, 0, 1}
var switchSubnetMask = net.CIDRMask(24, 32)

// defaultLeaseOptions sets the lease options needed to properly configure connectivity to nanoswitch
func defaultLeaseOptions(reply *dhcpv4.DHCPv4) {
	reply.GatewayIPAddr = switchIP
	reply.UpdateOption(dhcpv4.OptDNS(net.IPv4(10, 42, 0, 3))) // SLIRP fake DNS server
	reply.UpdateOption(dhcpv4.OptRouter(switchIP))
	reply.IPAddressLeaseTime(12 * time.Hour)
	reply.UpdateOption(dhcpv4.OptSubnetMask(switchSubnetMask))
}

// runDHCPServer runs an extremely minimal DHCP server with most options hardcoded, a wrapping bump allocator for the
// IPs, 12h Lease timeout and no support for DHCP collision detection.
func runDHCPServer(link netlink.Link) supervisor.Runnable {
	currentIP := net.IP{10, 1, 0, 1}

	return func(ctx context.Context) error {
		laddr := net.UDPAddr{
			IP:   net.IPv4(0, 0, 0, 0),
			Port: 67,
		}
		server, err := server4.NewServer(link.Attrs().Name, &laddr, func(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
			if m == nil {
				return
			}
			reply, err := dhcpv4.NewReplyFromRequest(m)
			if err != nil {
				supervisor.Logger(ctx).Warn("Failed to generate DHCP reply", zap.Error(err))
				return
			}
			reply.UpdateOption(dhcpv4.OptServerIdentifier(switchIP))
			reply.ServerIPAddr = switchIP

			switch m.MessageType() {
			case dhcpv4.MessageTypeDiscover:
				reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
				defaultLeaseOptions(reply)
				currentIP[3]++ // Works only because it's a /24
				reply.YourIPAddr = currentIP
				supervisor.Logger(ctx).Info("Replying with DHCP IP", zap.String("ip", reply.YourIPAddr.String()))
			case dhcpv4.MessageTypeRequest:
				reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
				defaultLeaseOptions(reply)
				reply.YourIPAddr = m.RequestedIPAddress()
			case dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline:
				supervisor.Logger(ctx).Info("Ignoring Release/Decline")
			}
			if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
				supervisor.Logger(ctx).Warn("Cannot reply to client", zap.Error(err))
			}
		})
		if err != nil {
			return err
		}
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		go func() {
			<-ctx.Done()
			server.Close()
		}()
		return server.Serve()
	}
}

// userspaceProxy listens on port and proxies all TCP connections to the same port on targetIP
func userspaceProxy(targetIP net.IP, port uint16) supervisor.Runnable {
	return func(ctx context.Context) error {
		logger := supervisor.Logger(ctx)
		tcpListener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(0, 0, 0, 0), Port: int(port)})
		if err != nil {
			return err
		}
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		go func() {
			<-ctx.Done()
			tcpListener.Close()
		}()
		for {
			conn, err := tcpListener.AcceptTCP()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			go func(conn *net.TCPConn) {
				defer conn.Close()
				upstreamConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: targetIP, Port: int(port)})
				if err != nil {
					logger.Info("Userspace proxy failed to connect to upstream", zap.Error(err))
					return
				}
				defer upstreamConn.Close()
				go io.Copy(upstreamConn, conn)
				io.Copy(conn, upstreamConn)
			}(conn)
		}

	}
}

// addNetworkRoutes sets up routing from DHCP
func addNetworkRoutes(link netlink.Link, addr net.IPNet, gw net.IP) error {
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: &addr}); err != nil {
		return fmt.Errorf("failed to add DHCP address to network interface \"%v\": %w", link.Attrs().Name, err)
	}

	if gw.IsUnspecified() {
		return nil
	}

	route := &netlink.Route{
		Dst:   &net.IPNet{IP: net.IPv4(0, 0, 0, 0), Mask: net.IPv4Mask(0, 0, 0, 0)},
		Gw:    gw,
		Scope: netlink.SCOPE_UNIVERSE,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("could not add default route: netlink.RouteAdd(%+v): %v", route, err)
	}
	return nil
}

// nfifname converts an interface name into 16 bytes padded with zeroes (for nftables)
func nfifname(n string) []byte {
	b := make([]byte, 16)
	copy(b, []byte(n+"\x00"))
	return b
}

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	supervisor.New(context.Background(), logger, func(ctx context.Context) error {
		logger := supervisor.Logger(ctx)
		logger.Info("Starting NanoSwitch, a tiny TOR switch emulator")

		// Set up target filesystems.
		for _, el := range []struct {
			dir   string
			fs    string
			flags uintptr
		}{
			{"/sys", "sysfs", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV},
			{"/proc", "proc", unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_NODEV},
			{"/dev", "devtmpfs", unix.MS_NOEXEC | unix.MS_NOSUID},
			{"/dev/pts", "devpts", unix.MS_NOEXEC | unix.MS_NOSUID},
		} {
			if err := os.Mkdir(el.dir, 0755); err != nil && !os.IsExist(err) {
				return fmt.Errorf("could not make %s: %w", el.dir, err)
			}
			if err := unix.Mount(el.fs, el.dir, el.fs, el.flags, ""); err != nil {
				return fmt.Errorf("could not mount %s on %s: %w", el.fs, el.dir, err)
			}
		}

		c := &nftables.Conn{}

		links, err := netlink.LinkList()
		if err != nil {
			logger.Panic("Failed to list links", zap.Error(err))
		}
		var externalLink netlink.Link
		var vmLinks []netlink.Link
		for _, link := range links {
			attrs := link.Attrs()
			if link.Type() == "device" && len(attrs.HardwareAddr) > 0 {
				if attrs.Flags&net.FlagUp != net.FlagUp {
					netlink.LinkSetUp(link) // Attempt to take up all ethernet links
				}
				if bytes.Equal(attrs.HardwareAddr, launch.HostInterfaceMAC) {
					externalLink = link
				} else {
					vmLinks = append(vmLinks, link)
				}
			}
		}
		vmBridgeLink := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "vmbridge", Flags: net.FlagUp}}
		if err := netlink.LinkAdd(vmBridgeLink); err != nil {
			logger.Panic("Failed to create vmbridge", zap.Error(err))
		}
		for _, link := range vmLinks {
			if err := netlink.LinkSetMaster(link, vmBridgeLink); err != nil {
				logger.Panic("Failed to add VM interface to bridge", zap.Error(err))
			}
			logger.Info("Assigned interface to bridge", zap.String("if", link.Attrs().Name))
		}
		if err := netlink.AddrReplace(vmBridgeLink, &netlink.Addr{IPNet: &net.IPNet{IP: switchIP, Mask: switchSubnetMask}}); err != nil {
			logger.Panic("Failed to assign static IP to vmbridge")
		}
		if externalLink != nil {
			nat := c.AddTable(&nftables.Table{
				Family: nftables.TableFamilyIPv4,
				Name:   "nat",
			})

			postrouting := c.AddChain(&nftables.Chain{
				Name:     "postrouting",
				Hooknum:  nftables.ChainHookPostrouting,
				Priority: nftables.ChainPriorityNATSource,
				Table:    nat,
				Type:     nftables.ChainTypeNAT,
			})

			// Masquerade/SNAT all traffic going out of the external interface
			c.AddRule(&nftables.Rule{
				Table: nat,
				Chain: postrouting,
				Exprs: []expr.Any{
					&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
					&expr.Cmp{
						Op:       expr.CmpOpEq,
						Register: 1,
						Data:     nfifname(externalLink.Attrs().Name),
					},
					&expr.Masq{},
				},
			})

			if err := c.Flush(); err != nil {
				panic(err)
			}

			dhcpClient := dhcp.New()
			supervisor.Run(ctx, "dhcp-client", dhcpClient.Run(externalLink))
			if err := ioutil.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
				logger.Panic("Failed to write ip forwards", zap.Error(err))
			}
			status, err := dhcpClient.Status(ctx, true)
			if err != nil {
				return err
			}

			if err := addNetworkRoutes(externalLink, status.Address, status.Gateway); err != nil {
				return err
			}
		} else {
			logger.Info("No upstream interface detected")
		}
		supervisor.Run(ctx, "dhcp-server", runDHCPServer(vmBridgeLink))
		supervisor.Run(ctx, "proxy-ext1", userspaceProxy(net.IPv4(10, 1, 0, 2), common.ExternalServicePort))
		supervisor.Run(ctx, "proxy-dbg1", userspaceProxy(net.IPv4(10, 1, 0, 2), common.DebugServicePort))
		supervisor.Run(ctx, "proxy-k8s-api1", userspaceProxy(net.IPv4(10, 1, 0, 2), common.KubernetesAPIPort))
		supervisor.Signal(ctx, supervisor.SignalHealthy)
		supervisor.Signal(ctx, supervisor.SignalDone)
		return nil
	})
	select {}
}