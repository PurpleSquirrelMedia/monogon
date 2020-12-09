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

package callback

import (
	"fmt"
	"math"
	"net"
	"os"
	"testing"
	"time"

	"git.monogon.dev/source/nexantic.git/core/pkg/dhcp4c"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func trivialLeaseFromNet(ipnet net.IPNet) *dhcp4c.Lease {
	opts := make(dhcpv4.Options)
	opts.Update(dhcpv4.OptSubnetMask(ipnet.Mask))
	return &dhcp4c.Lease{
		AssignedIP: ipnet.IP,
		ExpiresAt:  time.Now().Add(1 * time.Second),
		Options:    opts,
	}
}

var (
	testNet1          = net.IPNet{IP: net.IP{10, 0, 1, 2}, Mask: net.CIDRMask(24, 32)}
	testNet1Broadcast = net.IP{10, 0, 1, 255}
	testNet1Router    = net.IP{10, 0, 1, 1}
	testNet2          = net.IPNet{IP: net.IP{10, 0, 2, 2}, Mask: net.CIDRMask(24, 32)}
	testNet2Broadcast = net.IP{10, 0, 2, 255}
	testNet2Router    = net.IP{10, 0, 2, 1}
	mainRoutingTable  = 254 // Linux automatically puts all routes into this table unless specified
)

func TestAssignedIPCallback(t *testing.T) {
	if os.Getenv("IN_KTEST") != "true" {
		t.Skip("Not in ktest")
	}

	var tests = []struct {
		name               string
		initialAddrs       []netlink.Addr
		oldLease, newLease *dhcp4c.Lease
		expectedAddrs      []netlink.Addr
	}{
		{ // Lifetimes are necessary, otherwise the Kernel sets the IFA_F_PERMANENT flag behind our back
			name:          "RemoveOldIPs",
			initialAddrs:  []netlink.Addr{{IPNet: &testNet1, ValidLft: 60}, {IPNet: &testNet2, ValidLft: 60}},
			oldLease:      nil,
			newLease:      nil,
			expectedAddrs: nil,
		},
		{
			name:         "IgnoresPermanentIPs",
			initialAddrs: []netlink.Addr{{IPNet: &testNet1, Flags: unix.IFA_F_PERMANENT}, {IPNet: &testNet2, ValidLft: 60}},
			oldLease:     nil,
			newLease:     trivialLeaseFromNet(testNet2),
			expectedAddrs: []netlink.Addr{
				{IPNet: &testNet1, Flags: unix.IFA_F_PERMANENT, ValidLft: math.MaxUint32, PreferedLft: math.MaxUint32, Broadcast: testNet1Broadcast},
				{IPNet: &testNet2, ValidLft: 1, PreferedLft: 1, Broadcast: testNet2Broadcast},
			},
		},
		{
			name:         "AssignsNewIP",
			initialAddrs: []netlink.Addr{},
			oldLease:     nil,
			newLease:     trivialLeaseFromNet(testNet2),
			expectedAddrs: []netlink.Addr{
				{IPNet: &testNet2, ValidLft: 1, PreferedLft: 1, Broadcast: testNet2Broadcast},
			},
		},
		{
			name:         "UpdatesIP",
			initialAddrs: []netlink.Addr{},
			oldLease:     trivialLeaseFromNet(testNet2),
			newLease:     trivialLeaseFromNet(testNet1),
			expectedAddrs: []netlink.Addr{
				{IPNet: &testNet1, ValidLft: 1, PreferedLft: 1, Broadcast: testNet1Broadcast},
			},
		},
		{
			name:          "RemovesIPOnRelease",
			initialAddrs:  []netlink.Addr{{IPNet: &testNet1, ValidLft: 60, PreferedLft: 60}},
			oldLease:      trivialLeaseFromNet(testNet1),
			newLease:      nil,
			expectedAddrs: nil,
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testLink := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  fmt.Sprintf("aipcb-test-%d", i),
					Flags: unix.IFF_UP,
				},
			}
			if err := netlink.LinkAdd(testLink); err != nil {
				t.Fatalf("test cannot set up network interface: %v", err)
			}
			defer netlink.LinkDel(testLink)
			for _, addr := range test.initialAddrs {
				if err := netlink.AddrAdd(testLink, &addr); err != nil {
					t.Fatalf("test cannot set up initial addrs: %v", err)
				}
			}
			// Associate dynamically-generated interface name for later comparison
			for i := range test.expectedAddrs {
				test.expectedAddrs[i].Label = testLink.Name
			}
			cb := ManageIP(testLink)
			if err := cb(test.oldLease, test.newLease); err != nil {
				t.Fatalf("callback returned an error: %v", err)
			}
			addrs, err := netlink.AddrList(testLink, netlink.FAMILY_V4)
			if err != nil {
				t.Fatalf("test cannot read back addrs from interface: %v", err)
			}
			require.Equal(t, test.expectedAddrs, addrs, "Wrong IPs on interface")
		})
	}
}

func leaseAddRouter(lease *dhcp4c.Lease, router net.IP) *dhcp4c.Lease {
	lease.Options.Update(dhcpv4.OptRouter(router))
	return lease
}

func TestDefaultRouteCallback(t *testing.T) {
	if os.Getenv("IN_KTEST") != "true" {
		t.Skip("Not in ktest")
	}
	// testRoute is only used as a route destination and not configured on any interface.
	testRoute := net.IPNet{IP: net.IP{10, 0, 3, 0}, Mask: net.CIDRMask(24, 32)}

	// A test interface is set up for each test and assigned testNet1 and testNet2 so that testNet1Router and
	// testNet2Router are valid gateways for routes in this environment. A LinkIndex of -1 is replaced by the correct
	// link index for this test interface at runtime for both initialRoutes and expectedRoutes.
	var tests = []struct {
		name               string
		initialRoutes      []netlink.Route
		oldLease, newLease *dhcp4c.Lease
		expectedRoutes     []netlink.Route
	}{
		{
			name:          "AddsDefaultRoute",
			initialRoutes: []netlink.Route{},
			oldLease:      nil,
			newLease:      leaseAddRouter(trivialLeaseFromNet(testNet1), testNet1Router),
			expectedRoutes: []netlink.Route{{
				Protocol:  unix.RTPROT_DHCP,
				Dst:       nil, // Linux weirdly retuns no RTA_DST for default routes, but one for everything else
				Gw:        testNet1Router,
				Src:       testNet1.IP,
				Table:     mainRoutingTable,
				LinkIndex: -1, // Filled in dynamically with test interface
				Type:      unix.RTN_UNICAST,
			}},
		},
		{
			name:           "IgnoresLeasesWithoutRouter",
			initialRoutes:  []netlink.Route{},
			oldLease:       nil,
			newLease:       trivialLeaseFromNet(testNet1),
			expectedRoutes: nil,
		},
		{
			name: "RemovesUnrelatedOldRoutes",
			initialRoutes: []netlink.Route{{
				Dst:       &testRoute,
				LinkIndex: -1, // Filled in dynamically with test interface
				Protocol:  unix.RTPROT_DHCP,
				Gw:        testNet2Router,
				Scope:     netlink.SCOPE_UNIVERSE,
			}},
			oldLease:       nil,
			newLease:       nil,
			expectedRoutes: nil,
		},
		{
			name: "IgnoresNonDHCPRoutes",
			initialRoutes: []netlink.Route{{
				Dst:       &testRoute,
				LinkIndex: -1, // Filled in dynamically with test interface
				Protocol:  unix.RTPROT_BIRD,
				Gw:        testNet2Router,
			}},
			oldLease: trivialLeaseFromNet(testNet1),
			newLease: nil,
			expectedRoutes: []netlink.Route{{
				Protocol:  unix.RTPROT_BIRD,
				Dst:       &testRoute,
				Gw:        testNet2Router,
				Table:     mainRoutingTable,
				LinkIndex: -1, // Filled in dynamically with test interface
				Type:      unix.RTN_UNICAST,
			}},
		},
		{
			name: "RemovesRoute",
			initialRoutes: []netlink.Route{{
				Dst:       nil,
				LinkIndex: -1, // Filled in dynamically with test interface
				Protocol:  unix.RTPROT_DHCP,
				Gw:        testNet2Router,
			}},
			oldLease:       leaseAddRouter(trivialLeaseFromNet(testNet2), testNet2Router),
			newLease:       nil,
			expectedRoutes: nil,
		},
		{
			name: "UpdatesRoute",
			initialRoutes: []netlink.Route{{
				Dst:       nil,
				LinkIndex: -1, // Filled in dynamically with test interface
				Protocol:  unix.RTPROT_DHCP,
				Src:       testNet1.IP,
				Gw:        testNet1Router,
			}},
			oldLease: leaseAddRouter(trivialLeaseFromNet(testNet1), testNet1Router),
			newLease: leaseAddRouter(trivialLeaseFromNet(testNet2), testNet2Router),
			expectedRoutes: []netlink.Route{{
				Protocol:  unix.RTPROT_DHCP,
				Dst:       nil,
				Gw:        testNet2Router,
				Src:       testNet2.IP,
				Table:     mainRoutingTable,
				LinkIndex: -1, // Filled in dynamically with test interface
				Type:      unix.RTN_UNICAST,
			}},
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testLink := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:  fmt.Sprintf("drcb-test-%d", i),
					Flags: unix.IFF_UP,
				},
			}
			if err := netlink.LinkAdd(testLink); err != nil {
				t.Fatalf("test cannot set up network interface: %v", err)
			}
			defer func() { // Clean up after each test
				routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{}, 0)
				if err == nil {
					for _, route := range routes {
						netlink.RouteDel(&route)
					}
				}
			}()
			defer netlink.LinkDel(testLink)
			if err := netlink.AddrAdd(testLink, &netlink.Addr{
				IPNet: &testNet1,
			}); err != nil {
				t.Fatalf("test cannot set up test addrs: %v", err)
			}
			if err := netlink.AddrAdd(testLink, &netlink.Addr{
				IPNet: &testNet2,
			}); err != nil {
				t.Fatalf("test cannot set up test addrs: %v", err)
			}
			for _, route := range test.initialRoutes {
				if route.LinkIndex == -1 {
					route.LinkIndex = testLink.Index
				}
				if err := netlink.RouteAdd(&route); err != nil {
					t.Fatalf("test cannot set up initial routes: %v", err)
				}
			}
			for i := range test.expectedRoutes {
				if test.expectedRoutes[i].LinkIndex == -1 {
					test.expectedRoutes[i].LinkIndex = testLink.Index
				}
			}

			cb := ManageDefaultRoute(testLink)
			if err := cb(test.oldLease, test.newLease); err != nil {
				t.Fatalf("callback returned an error: %v", err)
			}
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{}, 0)
			if err != nil {
				t.Fatalf("test cannot read back routes: %v", err)
			}
			var notKernelRoutes []netlink.Route
			for _, route := range routes {
				if route.Protocol != unix.RTPROT_KERNEL { // Filter kernel-managed routes
					notKernelRoutes = append(notKernelRoutes, route)
				}
			}
			require.Equal(t, test.expectedRoutes, notKernelRoutes, "Wrong Routes")
		})
	}
}
