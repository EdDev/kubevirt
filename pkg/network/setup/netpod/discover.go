/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2023 Red Hat, Inc.
 *
 */

package netpod

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	vishnetlink "github.com/vishvananda/netlink"

	knet "k8s.io/utils/net"

	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"

	"kubevirt.io/kubevirt/pkg/network/cache"
	"kubevirt.io/kubevirt/pkg/network/driver/nmstate"
	"kubevirt.io/kubevirt/pkg/network/namescheme"
)

// discover goes over the current pod network configuration and persists/caches
// the relevant data (for recovery and data sharing).
func (n NetPod) discover(currentStatus *nmstate.Status) error {
	podIfaceStatusByName := ifaceStatusByName(currentStatus.Interfaces)
	podIfaceNameByVMINetwork := createNetworkNameScheme(n.vmiSpecNets, currentStatus.Interfaces)

	for _, vmiSpecIface := range n.vmiSpecIfaces {
		podIfaceName := podIfaceNameByVMINetwork[vmiSpecIface.Name]

		// Filter out network interfaces marked for removal.
		// TODO: Support in the same flow the removal of such interfaces.
		if vmiSpecIface.State == v1.InterfaceStateAbsent && !namescheme.OrdinalSecondaryInterfaceName(vmiSpecIface.Name) {
			continue
		}

		podIfaceStatus, podIfaceExists := podIfaceStatusByName[podIfaceName]

		switch {
		case vmiSpecIface.Bridge != nil:
			if !podIfaceExists {
				return fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}

			if err := n.storePodInterfaceData(vmiSpecIface, podIfaceStatus); err != nil {
				return err
			}

			if err := n.storeBridgeBindingDHCPInterfaceData(currentStatus, podIfaceStatus, vmiSpecIface, podIfaceName); err != nil {
				return err
			}

			if err := n.storeBridgeDomainInterfaceData(podIfaceStatus, vmiSpecIface); err != nil {
				return err
			}

		case vmiSpecIface.Masquerade != nil:
			if !podIfaceExists {
				return fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}

			if err := n.storePodInterfaceData(vmiSpecIface, podIfaceStatus); err != nil {
				return err
			}

		case vmiSpecIface.Passt != nil:
			if !podIfaceExists {
				return fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}

			if err := n.storePodInterfaceData(vmiSpecIface, podIfaceStatus); err != nil {
				return err
			}

		case vmiSpecIface.Slirp != nil:
			if !podIfaceExists {
				return fmt.Errorf("pod link (%s) is missing", podIfaceName)
			}

			if err := n.storePodInterfaceData(vmiSpecIface, podIfaceStatus); err != nil {
				return err
			}

		// Skip the discovery for all other known network interface bindings.
		case vmiSpecIface.Binding != nil:
		case vmiSpecIface.Macvtap != nil:
		case vmiSpecIface.SRIOV != nil:
		default:
			return fmt.Errorf("undefined binding method: %v", vmiSpecIface)
		}
	}
	return nil
}

func (n NetPod) storePodInterfaceData(vmiSpecIface v1.Interface, ifaceState nmstate.Interface) error {
	ifCache, err := cache.ReadPodInterfaceCache(n.cacheCreator, n.vmiUID, vmiSpecIface.Name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to read pod interface cache for %s: %v", vmiSpecIface.Name, err)
		}
		ifCache = &cache.PodIfaceCacheData{}
	}

	ifCache.Iface = &vmiSpecIface

	ipv4 := firstIPGlobalUnicast(ifaceState.IPv4)
	ipv6 := firstIPGlobalUnicast(ifaceState.IPv6)
	switch {
	case ipv4 != nil && ipv6 != nil:
		ifCache.PodIPs, err = sortIPsBasedOnPrimaryIP(ipv4.IP, ipv6.IP)
		if err != nil {
			return err
		}
	case ipv4 != nil:
		ifCache.PodIPs = []string{ipv4.IP}
	case ipv6 != nil:
		ifCache.PodIPs = []string{ipv6.IP}
	default:
		return nil
	}
	ifCache.PodIP = ifCache.PodIPs[0]

	if err := cache.WritePodInterfaceCache(n.cacheCreator, n.vmiUID, vmiSpecIface.Name, ifCache); err != nil {
		log.Log.Reason(err).Errorf("failed to write pod interface data to cache")
		return err
	}
	return nil
}

func (n NetPod) storeBridgeBindingDHCPInterfaceData(currentStatus *nmstate.Status, podIfaceStatus nmstate.Interface, vmiSpecIface v1.Interface, podIfaceName string) error {
	var dhcpConfig cache.DHCPConfig
	dhcpConfig.IPAMDisabled = true
	if ipAddress := firstIPGlobalUnicast(podIfaceStatus.IPv4); ipAddress != nil {
		dhcpConfig.IPAMDisabled = false

		addr, iperr := vishnetlink.ParseAddr(fmt.Sprintf("%s/%d", ipAddress.IP, ipAddress.PrefixLen))
		if iperr != nil {
			return iperr
		}
		dhcpConfig.IP = *addr

		mac, err := resolveMacAddress(podIfaceStatus.MacAddress, vmiSpecIface.MacAddress)
		if err != nil {
			return err
		}
		dhcpConfig.MAC = mac

		linkRoutes, err := filterIPv4RoutesByInterface(currentStatus, podIfaceName)
		if err != nil {
			return err
		}
		dhcpConfig.Gateway = net.ParseIP(linkRoutes[0].NextHopAddress)

		otherRoutes, err := filterRoutesByNonLocalDestination(linkRoutes, addr)
		if err != nil {
			return err
		}

		dhcpRoutes, err := translateNmstateToNetlinkRoutes(otherRoutes)
		if err != nil {
			return err
		}
		if len(dhcpRoutes) > 0 {
			dhcpConfig.Routes = &dhcpRoutes
		}
	}

	log.Log.V(4).Infof("The generated dhcpConfig: %s\nRoutes: %+v", dhcpConfig.String(), dhcpConfig.Routes)
	if err := cache.WriteDHCPInterfaceCache(n.cacheCreator, strconv.Itoa(n.podPID), podIfaceName, &dhcpConfig); err != nil {
		return fmt.Errorf("failed to save DHCP configuration: %v", err)
	}

	return nil
}

func (n NetPod) storeBridgeDomainInterfaceData(podIfaceStatus nmstate.Interface, vmiSpecIface v1.Interface) error {
	mac, err := resolveMacAddress(podIfaceStatus.MacAddress, vmiSpecIface.MacAddress)
	if err != nil {
		return err
	}

	domainIface := api.Interface{MAC: &api.MAC{MAC: mac.String()}}

	log.Log.V(4).Infof("The generated domain interface data: mac = %s", domainIface.MAC.MAC)
	if err := cache.WriteDomainInterfaceCache(n.cacheCreator, strconv.Itoa(n.podPID), vmiSpecIface.Name, &domainIface); err != nil {
		return fmt.Errorf("failed to save domain interface data: %v", err)
	}

	return nil
}

func translateNmstateToNetlinkRoutes(otherRoutes []nmstate.Route) ([]vishnetlink.Route, error) {
	var dhcpRoutes []vishnetlink.Route
	for _, nmstateRoute := range otherRoutes {
		isDefaultRoute := nmstateRoute.Destination == nmstate.DefaultDestinationRoute(vishnetlink.FAMILY_V4).String()
		var dstAddr *net.IPNet
		if !isDefaultRoute {
			_, ipNet, perr := net.ParseCIDR(nmstateRoute.Destination)
			if perr != nil {
				return nil, perr
			}
			dstAddr = ipNet
		}
		route := vishnetlink.Route{
			Dst: dstAddr,
			Gw:  net.ParseIP(nmstateRoute.NextHopAddress),
		}
		dhcpRoutes = append(dhcpRoutes, route)
	}
	return dhcpRoutes, nil
}

// filterRoutesByNonLocalDestination filters out local routes (the destination is of the local link).
// Default routes should not be filter out.
func filterRoutesByNonLocalDestination(linkRoutes []nmstate.Route, addr *vishnetlink.Addr) ([]nmstate.Route, error) {
	var otherRoutes []nmstate.Route
	for _, route := range linkRoutes {
		_, dstIPNet, perr := net.ParseCIDR(route.Destination)
		if perr != nil {
			return nil, perr
		}
		isDefaultRoute := route.Destination == nmstate.DefaultDestinationRoute(vishnetlink.FAMILY_V4).String()
		localDestination := dstIPNet.Contains(addr.IP)
		if isDefaultRoute || !localDestination {
			otherRoutes = append(otherRoutes, route)
		}
	}
	return otherRoutes, nil
}

func filterIPv4RoutesByInterface(currentStatus *nmstate.Status, podIfaceName string) ([]nmstate.Route, error) {
	var linkRoutes []nmstate.Route
	for _, route := range currentStatus.Routes.Running {
		ip, _, err := net.ParseCIDR(route.Destination)
		if err != nil {
			return nil, err
		}
		if isIPv6Family(ip) || isIPv6Family(net.ParseIP(route.NextHopAddress)) {
			continue
		}
		if route.NextHopInterface == podIfaceName {
			linkRoutes = append(linkRoutes, route)
		}
	}
	if len(linkRoutes) == 0 {
		return nil, fmt.Errorf("no gateway address found in routes for %s", podIfaceName)
	}
	return linkRoutes, nil
}

func resolveMacAddress(macAddressFromCurrent string, macAddressFromVMISpec string) (net.HardwareAddr, error) {
	macAddress := macAddressFromCurrent
	if macAddressFromVMISpec != "" {
		macAddress = macAddressFromVMISpec
	}
	mac, merr := net.ParseMAC(macAddress)
	if merr != nil {
		return nil, merr
	}
	return mac, nil
}

// sortIPsBasedOnPrimaryIP returns a sorted slice of IP/s based on the detected cluster primary IP.
// The operation clones the Pod status IP list order logic.
func sortIPsBasedOnPrimaryIP(ipv4, ipv6 string) ([]string, error) {
	ipv4Primary, err := isIPv4Primary()
	if err != nil {
		return nil, err
	}

	if ipv4Primary {
		return []string{ipv4, ipv6}, nil
	}
	return []string{ipv6, ipv4}, nil
}

// isIPv4Primary inspects the existence of `MY_POD_IP` environment variable which
// is added to the virt-handler on its pod definition.
// The value tracks the pod `status.podIP` field value which in turn determines
// what is the cluster primary stack (IPv4 or IPv6).
func isIPv4Primary() (bool, error) {
	podIP, exist := os.LookupEnv("MY_POD_IP")
	if !exist {
		return false, fmt.Errorf("MY_POD_IP doesnt exists")
	}

	return !knet.IsIPv6String(podIP), nil
}

func isIPv6Family(ip net.IP) bool {
	isIPv4 := len(ip) <= net.IPv4len || ip.To4() != nil
	return !isIPv4
}