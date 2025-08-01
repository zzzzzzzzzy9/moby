// Package resolvconf provides utility code to query and update DNS configuration in /etc/resolv.conf
package resolvconf

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/resolvconf"
	"github.com/opencontainers/go-digest"
)

// constants for the IP address type
const (
	IP = iota // IPv4 and IPv6
	IPv4
	IPv6
)

// File contains the resolv.conf content and its hash
type File struct {
	Content []byte
	Hash    []byte
}

func Path() string {
	return resolvconf.Path()
}

// Get returns the contents of /etc/resolv.conf and its hash
func Get() (*File, error) {
	return GetSpecific(Path())
}

// GetSpecific returns the contents of the user specified resolv.conf file and its hash
func GetSpecific(path string) (*File, error) {
	resolv, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hash := digest.FromBytes(resolv)
	return &File{Content: resolv, Hash: []byte(hash)}, nil
}

// FilterResolvDNS cleans up the config in resolvConf.  It has two main jobs:
//  1. It looks for localhost (127.*|::1) entries in the provided
//     resolv.conf, removing local nameserver entries, and, if the resulting
//     cleaned config has no defined nameservers left, adds default DNS entries
//  2. Given the caller provides the enable/disable state of IPv6, the filter
//     code will remove all IPv6 nameservers if it is not enabled for containers
func FilterResolvDNS(resolvConf []byte, ipv6Enabled bool) (*File, error) {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil, err
	}
	rc.TransformForLegacyNw(ipv6Enabled)
	content, err := rc.Generate(false)
	if err != nil {
		return nil, err
	}
	hash := digest.FromBytes(content)
	return &File{Content: content, Hash: []byte(hash)}, nil
}

// GetNameservers returns nameservers (if any) listed in /etc/resolv.conf
func GetNameservers(resolvConf []byte, kind int) []string {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil
	}
	nsAddrs := rc.NameServers()
	var nameservers []string
	for _, addr := range nsAddrs {
		if kind == IP {
			nameservers = append(nameservers, addr.String())
		} else if kind == IPv4 && addr.Is4() {
			nameservers = append(nameservers, addr.String())
		} else if kind == IPv6 && addr.Is6() {
			nameservers = append(nameservers, addr.String())
		}
	}
	return nameservers
}

// GetNameserversAsPrefix returns nameservers (if any) listed in
// /etc/resolv.conf as CIDR blocks (e.g., "1.2.3.4/32")
func GetNameserversAsPrefix(resolvConf []byte) []netip.Prefix {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil
	}
	nsAddrs := rc.NameServers()
	nameservers := make([]netip.Prefix, 0, len(nsAddrs))
	for _, addr := range nsAddrs {
		nameservers = append(nameservers, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return nameservers
}

// GetSearchDomains returns search domains (if any) listed in /etc/resolv.conf
// If more than one search line is encountered, only the contents of the last
// one is returned.
func GetSearchDomains(resolvConf []byte) []string {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil
	}
	return rc.Search()
}

// GetOptions returns options (if any) listed in /etc/resolv.conf
// If more than one options line is encountered, only the contents of the last
// one is returned.
func GetOptions(resolvConf []byte) []string {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil
	}
	return rc.Options()
}

// Build generates and writes a configuration file to path containing a nameserver
// entry for every element in nameservers, a "search" entry for every element in
// dnsSearch, and an "options" entry for every element in dnsOptions. It returns
// a File containing the generated content and its (sha256) hash.
//
// Note that the resolv.conf file is written, but the hash file is not.
func Build(path string, nameservers, dnsSearch, dnsOptions []string) (*File, error) {
	var ns []netip.Addr
	for _, addr := range nameservers {
		ipAddr, err := netip.ParseAddr(addr)
		if err != nil {
			return nil, fmt.Errorf("bad nameserver address: %w", err)
		}
		ns = append(ns, ipAddr)
	}
	rc := resolvconf.ResolvConf{}
	rc.OverrideNameServers(ns)
	rc.OverrideSearch(dnsSearch)
	rc.OverrideOptions(dnsOptions)

	content, err := rc.Generate(false)
	if err != nil {
		return nil, err
	}

	// Write the resolv.conf file - it's bind-mounted into the container, so can't
	// move a temp file into place, just have to truncate and write it.
	//
	// TODO(thaJeztah): the Build function is currently only used by BuildKit, which only uses "File.Content", and doesn't require the file to be written.
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return nil, err
	}

	// TODO(thaJeztah): the Build function is currently only used by BuildKit, which does not use the Hash
	hash := digest.FromBytes(content)
	return &File{Content: content, Hash: []byte(hash)}, nil
}
