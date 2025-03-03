/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	gocni "github.com/containerd/go-cni"
	"github.com/containerd/nerdctl/pkg/clientutil"
	"github.com/containerd/nerdctl/pkg/dnsutil"
	"github.com/containerd/nerdctl/pkg/dnsutil/hostsstore"
	"github.com/containerd/nerdctl/pkg/idutil/containerwalker"
	"github.com/containerd/nerdctl/pkg/mountutil"
	"github.com/containerd/nerdctl/pkg/netutil"
	"github.com/containerd/nerdctl/pkg/netutil/nettype"
	"github.com/containerd/nerdctl/pkg/portutil"
	"github.com/containerd/nerdctl/pkg/resolvconf"
	"github.com/containerd/nerdctl/pkg/rootlessutil"
	"github.com/containerd/nerdctl/pkg/strutil"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func getNetworkSlice(cmd *cobra.Command) ([]string, error) {
	var netSlice = []string{}
	var networkSet = false
	if cmd.Flags().Lookup("network").Changed {
		network, err := cmd.Flags().GetStringSlice("network")
		if err != nil {
			return nil, err
		}
		netSlice = append(netSlice, network...)
		networkSet = true
	}
	if cmd.Flags().Lookup("net").Changed {
		net, err := cmd.Flags().GetStringSlice("net")
		if err != nil {
			return nil, err
		}
		netSlice = append(netSlice, net...)
		networkSet = true
	}

	if !networkSet {
		network, err := cmd.Flags().GetStringSlice("network")
		if err != nil {
			return nil, err
		}
		netSlice = append(netSlice, network...)
	}
	return netSlice, nil
}

func withCustomResolvConf(src string) func(context.Context, oci.Client, *containers.Container, *oci.Spec) error {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/etc/resolv.conf",
			Type:        "bind",
			Source:      src,
			Options:     []string{"bind", mountutil.DefaultPropagationMode}, // writable
		})
		return nil
	}
}

func withCustomEtcHostname(src string) func(context.Context, oci.Client, *containers.Container, *oci.Spec) error {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/etc/hostname",
			Type:        "bind",
			Source:      src,
			Options:     []string{"bind", mountutil.DefaultPropagationMode}, // writable
		})
		return nil
	}
}

func withCustomHosts(src string) func(context.Context, oci.Client, *containers.Container, *oci.Spec) error {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		s.Mounts = append(s.Mounts, specs.Mount{
			Destination: "/etc/hosts",
			Type:        "bind",
			Source:      src,
			Options:     []string{"bind", mountutil.DefaultPropagationMode}, // writable
		})
		return nil
	}
}

func generateNetOpts(cmd *cobra.Command, dataStore, stateDir, ns, id string) ([]oci.SpecOpts, []string, string, []gocni.PortMapping, string, error) {
	opts := []oci.SpecOpts{}
	portSlice, err := cmd.Flags().GetStringSlice("publish")
	if err != nil {
		return nil, nil, "", nil, "", err
	}
	ipAddress, err := cmd.Flags().GetString("ip")
	if err != nil {
		return nil, nil, "", nil, "", err
	}
	netSlice, err := getNetworkSlice(cmd)
	if err != nil {
		return nil, nil, "", nil, "", err
	}

	if (len(netSlice) == 0) && (ipAddress != "") {
		logrus.Warnf("You have assign an IP address %s but no network, So we will use the default network", ipAddress)
	}

	macAddress, err := getMACAddress(cmd, netSlice)
	if err != nil {
		return nil, nil, "", nil, "", err
	}

	ports := make([]gocni.PortMapping, 0)
	netType, err := nettype.Detect(netSlice)
	if err != nil {
		return nil, nil, "", nil, "", err
	}

	switch netType {
	case nettype.None:
		// NOP
		// Docker compatible: if macAddress is specified, set MAC address shall
		// not work but run command will success
	case nettype.Host:
		if macAddress != "" {
			return nil, nil, "", nil, "", errors.New("conflicting options: mac-address and the network mode")
		}
		opts = append(opts, oci.WithHostNamespace(specs.NetworkNamespace), oci.WithHostHostsFile, oci.WithHostResolvconf)
	case nettype.CNI:
		// We only verify flags and generate resolv.conf here.
		// The actual network is configured in the oci hook.
		if err := verifyCNINetwork(cmd, netSlice, macAddress); err != nil {
			return nil, nil, "", nil, "", err
		}

		if runtime.GOOS == "linux" {
			resolvConfPath := filepath.Join(stateDir, "resolv.conf")
			if err := buildResolvConf(cmd, resolvConfPath); err != nil {
				return nil, nil, "", nil, "", err
			}

			// the content of /etc/hosts is created in OCI Hook
			etcHostsPath, err := hostsstore.AllocHostsFile(dataStore, ns, id)
			if err != nil {
				return nil, nil, "", nil, "", err
			}
			opts = append(opts, withCustomResolvConf(resolvConfPath), withCustomHosts(etcHostsPath))
			for _, p := range portSlice {
				pm, err := portutil.ParseFlagP(p)
				if err != nil {
					return nil, nil, "", pm, "", err
				}
				ports = append(ports, pm...)
			}
		}
	case nettype.Container:
		if macAddress != "" {
			return nil, nil, "", nil, "", errors.New("conflicting options: mac-address and the network mode")
		}
		if err := verifyContainerNetwork(cmd, netSlice); err != nil {
			return nil, nil, "", nil, "", err
		}
		network := strings.Split(netSlice[0], ":")
		if len(network) != 2 {
			return nil, nil, "", nil, "", fmt.Errorf("invalid network: %s, should be \"container:<id|name>\"", netSlice[0])
		}
		containerName := network[1]
		namespace, err := cmd.Flags().GetString("namespace")
		if err != nil {
			return nil, nil, "", nil, "", err
		}
		address, err := cmd.Flags().GetString("address")
		if err != nil {
			return nil, nil, "", nil, "", err
		}
		client, ctx, cancel, err := clientutil.NewClient(cmd.Context(), namespace, address)
		if err != nil {
			return nil, nil, "", nil, "", err
		}
		defer cancel()

		walker := &containerwalker.ContainerWalker{
			Client: client,
			OnFound: func(ctx context.Context, found containerwalker.Found) error {
				if found.MatchCount > 1 {
					return fmt.Errorf("multiple containers found with prefix: %s", containerName)
				}
				containerID := found.Container.ID()

				conStateDir, err := getContainerStateDirPath(cmd, dataStore, containerID)
				if err != nil {
					return err
				}

				s, err := found.Container.Spec(ctx)
				if err != nil {
					return err
				}
				hostname := s.Hostname
				hostnamePath := filepath.Join(conStateDir, "hostname")
				resolvConfPath := filepath.Join(conStateDir, "resolv.conf")
				etcHostsPath := hostsstore.HostsPath(dataStore, ns, containerID)
				netNSPath, err := getContainerNetNSPath(ctx, found.Container)
				if err != nil {
					return err
				}
				opts = append(opts,
					oci.WithLinuxNamespace(specs.LinuxNamespace{
						Type: specs.NetworkNamespace,
						Path: netNSPath,
					}),
					withCustomResolvConf(resolvConfPath),
					withCustomHosts(etcHostsPath),
					oci.WithHostname(hostname),
					withCustomEtcHostname(hostnamePath),
				)
				// stored in labels with key "nerdctl/networks"
				netSlice = []string{fmt.Sprintf("container:%s", containerID)}
				return nil
			},
		}
		n, err := walker.Walk(ctx, containerName)
		if err != nil {
			return nil, nil, "", nil, "", err
		}
		if n == 0 {
			return nil, nil, "", nil, "", fmt.Errorf("no such container: %s", containerName)
		}
	default:
		return nil, nil, "", nil, "", fmt.Errorf("unexpected network type %v", netType)
	}
	return opts, netSlice, ipAddress, ports, macAddress, nil
}

func getContainerNetNSPath(ctx context.Context, c containerd.Container) (string, error) {
	task, err := c.Task(ctx, nil)
	if err != nil {
		return "", err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return "", err
	}
	if status.Status != containerd.Running {
		return "", fmt.Errorf("invalid target container: %s, should be running", c.ID())
	}
	return fmt.Sprintf("/proc/%d/ns/net", task.Pid()), nil
}

func verifyCNINetwork(cmd *cobra.Command, netSlice []string, macAddress string) error {
	cniPath, err := cmd.Flags().GetString("cni-path")
	if err != nil {
		return err
	}
	cniNetconfpath, err := cmd.Flags().GetString("cni-netconfpath")
	if err != nil {
		return err
	}
	e, err := netutil.NewCNIEnv(cniPath, cniNetconfpath, netutil.WithDefaultNetwork())
	if err != nil {
		return err
	}
	macValidNetworks := []string{"bridge", "macvlan"}
	netMap, err := e.NetworkMap()
	if err != nil {
		return err
	}
	for _, netstr := range netSlice {
		netConfig, ok := netMap[netstr]
		if !ok {
			return fmt.Errorf("network %s not found", netstr)
		}
		// if MAC address is specified, the type of the network
		// must be one of macValidNetworks
		netType := netConfig.Plugins[0].Network.Type
		if macAddress != "" && !strutil.InStringSlice(macValidNetworks, netType) {
			return fmt.Errorf("%s interfaces on network %s do not support --mac-address", netType, netstr)
		}
	}
	return nil
}

func verifyContainerNetwork(cmd *cobra.Command, netSlice []string) error {
	if cmd.Flags().Changed("publish") {
		return fmt.Errorf("conflicting options: port publishing and the container type network mode")
	}
	if cmd.Flags().Changed("hostname") {
		return fmt.Errorf("conflicting options: hostname and the network mode")
	}
	if cmd.Flags().Changed("dns") {
		return fmt.Errorf("conflicting options: dns and the network mode")
	}
	if cmd.Flags().Changed("add-host") {
		return fmt.Errorf("conflicting options: custom host-to-IP mapping and the network mode")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("currently '--network=container:<container>' can only works on linux")
	}
	if len(netSlice) > 1 {
		return fmt.Errorf("only one network allowed using '--network=container:<container>'")
	}
	return nil
}

func buildResolvConf(cmd *cobra.Command, resolvConfPath string) error {
	dnsValue, err := cmd.Flags().GetStringSlice("dns")
	if err != nil {
		return err
	}
	dnsSearchValue, err := cmd.Flags().GetStringSlice("dns-search")
	if err != nil {
		return err
	}
	var dnsOptionValue []string
	if dnsOpts, err := cmd.Flags().GetStringSlice("dns-opt"); err == nil {
		dnsOptionValue = append(dnsOptionValue, dnsOpts...)
	} else {
		return err
	}
	if dnsOpts, err := cmd.Flags().GetStringSlice("dns-option"); err == nil {
		dnsOptionValue = append(dnsOptionValue, dnsOpts...)
	} else {
		return err
	}

	slirp4Dns := []string{}
	if rootlessutil.IsRootlessChild() {
		slirp4Dns, err = dnsutil.GetSlirp4netnsDNS()
		if err != nil {
			return err
		}
	}

	var (
		nameServers   = strutil.DedupeStrSlice(dnsValue)
		searchDomains = strutil.DedupeStrSlice(dnsSearchValue)
		dnsOptions    = strutil.DedupeStrSlice(dnsOptionValue)
	)

	if len(nameServers) == 0 || len(searchDomains) == 0 || len(dnsOptions) == 0 {
		conf, err := resolvconf.Get()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			// if resolvConf file does't exist, using default resolvers
			conf = &resolvconf.File{}
			logrus.WithError(err).Debug("resolvConf file doesn't exist")
		}
		conf, err = resolvconf.FilterResolvDNS(conf.Content, true)
		if err != nil {
			return err
		}
		if len(searchDomains) == 0 {
			searchDomains = resolvconf.GetSearchDomains(conf.Content)
		}
		if len(nameServers) == 0 {
			nameServers = resolvconf.GetNameservers(conf.Content, resolvconf.IPv4)
		}
		if len(dnsOptions) == 0 {
			dnsOptions = resolvconf.GetOptions(conf.Content)
		}
	}

	if _, err := resolvconf.Build(resolvConfPath, append(slirp4Dns, nameServers...), searchDomains, dnsOptions); err != nil {
		return err
	}
	return nil
}

func getMACAddress(cmd *cobra.Command, netSlice []string) (string, error) {
	macAddress, err := cmd.Flags().GetString("mac-address")
	if err != nil {
		return "", err
	}
	if macAddress == "" {
		return "", nil
	}
	if _, err := net.ParseMAC(macAddress); err != nil {
		return "", err
	}
	return macAddress, nil
}
