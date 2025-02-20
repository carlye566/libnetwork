package bridge

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/ipallocator"
	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/options"
	"github.com/docker/libnetwork/osl"
	"github.com/docker/libnetwork/portmapper"
	"github.com/docker/libnetwork/types"
	"github.com/vishvananda/netlink"
)

const (
	networkType             = "bridge"
	vethPrefix              = "veth"
	vethLen                 = 7
	containerVethPrefix     = "eth"
	maxAllocatePortAttempts = 10
	ifaceID                 = 1
)

var (
	ipAllocator *ipallocator.IPAllocator
)

// configuration info for the "bridge" driver.
type configuration struct {
	EnableIPForwarding  bool
	EnableIPTables      bool
	EnableUserlandProxy bool
}

// networkConfiguration for network specific configuration
type networkConfiguration struct {
	BridgeName            string
	AddressIPv4           *net.IPNet
	FixedCIDR             *net.IPNet
	FixedCIDRv6           *net.IPNet
	EnableIPv6            bool
	EnableIPMasquerade    bool
	EnableICC             bool
	Mtu                   int
	DefaultGatewayIPv4    net.IP
	DefaultGatewayIPv6    net.IP
	DefaultBindingIP      net.IP
	AllowNonDefaultBridge bool
}

// endpointConfiguration represents the user specified configuration for the sandbox endpoint
type endpointConfiguration struct {
	MacAddress   net.HardwareAddr
	PortBindings []types.PortBinding
	ExposedPorts []types.TransportPort
}

// containerConfiguration represents the user specified configuration for a container
type containerConfiguration struct {
	ParentEndpoints []string
	ChildEndpoints  []string
}

type bridgeEndpoint struct {
	id              string
	srcName         string
	addr            *net.IPNet
	addrv6          *net.IPNet
	macAddress      net.HardwareAddr
	config          *endpointConfiguration // User specified parameters
	containerConfig *containerConfiguration
	portMapping     []types.PortBinding // Operation port bindings
}

type bridgeNetwork struct {
	id         string
	bridge     *bridgeInterface // The bridge's L3 interface
	config     *networkConfiguration
	endpoints  map[string]*bridgeEndpoint // key: endpoint id
	portMapper *portmapper.PortMapper
	driver     *driver // The network's driver
	sync.Mutex
}

type driver struct {
	config      *configuration
	network     *bridgeNetwork
	natChain    *iptables.ChainInfo
	filterChain *iptables.ChainInfo
	networks    map[string]*bridgeNetwork
	sync.Mutex
}

// New constructs a new bridge driver
func newDriver() driverapi.Driver {
	ipAllocator = ipallocator.New()
	return &driver{networks: map[string]*bridgeNetwork{}}
}

// Init registers a new instance of bridge driver
func Init(dc driverapi.DriverCallback) error {
	if _, err := os.Stat("/proc/sys/net/bridge"); err != nil {
		if out, err := exec.Command("modprobe", "-va", "bridge", "br_netfilter").CombinedOutput(); err != nil {
			logrus.Warnf("Running modprobe bridge br_netfilter failed with message: %s, error: %v", out, err)
		}
	}
	if out, err := exec.Command("modprobe", "-va", "nf_nat").CombinedOutput(); err != nil {
		logrus.Warnf("Running modprobe nf_nat failed with message: `%s`, error: %v", strings.TrimSpace(string(out)), err)
	}
	if err := iptables.FirewalldInit(); err != nil {
		logrus.Debugf("Fail to initialize firewalld: %v, using raw iptables instead", err)
	}
	if err := iptables.RemoveExistingChain(DockerChain, iptables.Nat); err != nil {
		logrus.Warnf("Failed to remove existing iptables entries in %s : %v", DockerChain, err)
	}

	c := driverapi.Capability{
		Scope: driverapi.LocalScope,
	}
	return dc.RegisterDriver(networkType, newDriver(), c)
}

// Validate performs a static validation on the network configuration parameters.
// Whatever can be assessed a priori before attempting any programming.
func (c *networkConfiguration) Validate() error {
	if c.Mtu < 0 {
		return ErrInvalidMtu(c.Mtu)
	}

	// If bridge v4 subnet is specified
	if c.AddressIPv4 != nil {
		// If Container restricted subnet is specified, it must be a subset of bridge subnet
		if c.FixedCIDR != nil {
			// Check Network address
			if !c.AddressIPv4.Contains(c.FixedCIDR.IP) {
				return &ErrInvalidContainerSubnet{}
			}
			// Check it is effectively a subset
			brNetLen, _ := c.AddressIPv4.Mask.Size()
			cnNetLen, _ := c.FixedCIDR.Mask.Size()
			if brNetLen > cnNetLen {
				return &ErrInvalidContainerSubnet{}
			}
		}
		// If default gw is specified, it must be part of bridge subnet
		if c.DefaultGatewayIPv4 != nil {
			if !c.AddressIPv4.Contains(c.DefaultGatewayIPv4) {
				return &ErrInvalidGateway{}
			}
		}
	}

	// If default v6 gw is specified, FixedCIDRv6 must be specified and gw must belong to FixedCIDRv6 subnet
	if c.EnableIPv6 && c.DefaultGatewayIPv6 != nil {
		if c.FixedCIDRv6 == nil || !c.FixedCIDRv6.Contains(c.DefaultGatewayIPv6) {
			return &ErrInvalidGateway{}
		}
	}

	return nil
}

// Conflicts check if two NetworkConfiguration objects overlap
func (c *networkConfiguration) Conflicts(o *networkConfiguration) bool {
	if o == nil {
		return false
	}

	// Also empty, becasue only one network with empty name is allowed
	if c.BridgeName == o.BridgeName {
		return true
	}

	// They must be in different subnets
	if (c.AddressIPv4 != nil && o.AddressIPv4 != nil) &&
		(c.AddressIPv4.Contains(o.AddressIPv4.IP) || o.AddressIPv4.Contains(c.AddressIPv4.IP)) {
		return true
	}

	return false
}

// fromMap retrieve the configuration data from the map form.
func (c *networkConfiguration) fromMap(data map[string]interface{}) error {
	var err error

	if i, ok := data["BridgeName"]; ok && i != nil {
		if c.BridgeName, ok = i.(string); !ok {
			return types.BadRequestErrorf("invalid type for BridgeName value")
		}
	}

	if i, ok := data["Mtu"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.Mtu, err = strconv.Atoi(s); err != nil {
				return types.BadRequestErrorf("failed to parse Mtu value: %s", err.Error())
			}
		} else {
			return types.BadRequestErrorf("invalid type for Mtu value")
		}
	}

	if i, ok := data["EnableIPv6"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.EnableIPv6, err = strconv.ParseBool(s); err != nil {
				return types.BadRequestErrorf("failed to parse EnableIPv6 value: %s", err.Error())
			}
		} else {
			return types.BadRequestErrorf("invalid type for EnableIPv6 value")
		}
	}

	if i, ok := data["EnableIPMasquerade"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.EnableIPMasquerade, err = strconv.ParseBool(s); err != nil {
				return types.BadRequestErrorf("failed to parse EnableIPMasquerade value: %s", err.Error())
			}
		} else {
			return types.BadRequestErrorf("invalid type for EnableIPMasquerade value")
		}
	}

	if i, ok := data["EnableICC"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.EnableICC, err = strconv.ParseBool(s); err != nil {
				return types.BadRequestErrorf("failed to parse EnableICC value: %s", err.Error())
			}
		} else {
			return types.BadRequestErrorf("invalid type for EnableICC value")
		}
	}

	if i, ok := data["AllowNonDefaultBridge"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.AllowNonDefaultBridge, err = strconv.ParseBool(s); err != nil {
				return types.BadRequestErrorf("failed to parse AllowNonDefaultBridge value: %s", err.Error())
			}
		} else {
			return types.BadRequestErrorf("invalid type for AllowNonDefaultBridge value")
		}
	}

	if i, ok := data["AddressIPv4"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if ip, nw, e := net.ParseCIDR(s); e == nil {
				nw.IP = ip
				c.AddressIPv4 = nw
			} else {
				return types.BadRequestErrorf("failed to parse AddressIPv4 value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for AddressIPv4 value")
		}
	}

	if i, ok := data["FixedCIDR"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if ip, nw, e := net.ParseCIDR(s); e == nil {
				nw.IP = ip
				c.FixedCIDR = nw
			} else {
				return types.BadRequestErrorf("failed to parse FixedCIDR value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for FixedCIDR value")
		}
	}

	if i, ok := data["FixedCIDRv6"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if ip, nw, e := net.ParseCIDR(s); e == nil {
				nw.IP = ip
				c.FixedCIDRv6 = nw
			} else {
				return types.BadRequestErrorf("failed to parse FixedCIDRv6 value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for FixedCIDRv6 value")
		}
	}

	if i, ok := data["DefaultGatewayIPv4"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.DefaultGatewayIPv4 = net.ParseIP(s); c.DefaultGatewayIPv4 == nil {
				return types.BadRequestErrorf("failed to parse DefaultGatewayIPv4 value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for DefaultGatewayIPv4 value")
		}
	}

	if i, ok := data["DefaultGatewayIPv6"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.DefaultGatewayIPv6 = net.ParseIP(s); c.DefaultGatewayIPv6 == nil {
				return types.BadRequestErrorf("failed to parse DefaultGatewayIPv6 value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for DefaultGatewayIPv6 value")
		}
	}

	if i, ok := data["DefaultBindingIP"]; ok && i != nil {
		if s, ok := i.(string); ok {
			if c.DefaultBindingIP = net.ParseIP(s); c.DefaultBindingIP == nil {
				return types.BadRequestErrorf("failed to parse DefaultBindingIP value")
			}
		} else {
			return types.BadRequestErrorf("invalid type for DefaultBindingIP value")
		}
	}
	return nil
}

func (n *bridgeNetwork) getDriverChains() (*iptables.ChainInfo, *iptables.ChainInfo, error) {
	n.Lock()
	defer n.Unlock()

	if n.driver == nil {
		return nil, nil, types.BadRequestErrorf("no driver found")
	}

	return n.driver.natChain, n.driver.filterChain, nil
}

func (n *bridgeNetwork) getNetworkBridgeName() string {
	n.Lock()
	config := n.config
	n.Unlock()

	return config.BridgeName
}

func (n *bridgeNetwork) getEndpoint(eid string) (*bridgeEndpoint, error) {
	n.Lock()
	defer n.Unlock()

	if eid == "" {
		return nil, InvalidEndpointIDError(eid)
	}

	if ep, ok := n.endpoints[eid]; ok {
		return ep, nil
	}

	return nil, nil
}

// Install/Removes the iptables rules needed to isolate this network
// from each of the other networks
func (n *bridgeNetwork) isolateNetwork(others []*bridgeNetwork, enable bool) error {
	n.Lock()
	thisV4 := n.bridge.bridgeIPv4
	thisV6 := getV6Network(n.config, n.bridge)
	n.Unlock()

	// Install the rules to isolate this networks against each of the other networks
	for _, o := range others {
		o.Lock()
		otherV4 := o.bridge.bridgeIPv4
		otherV6 := getV6Network(o.config, o.bridge)
		o.Unlock()

		if !types.CompareIPNet(thisV4, otherV4) {
			// It's ok to pass a.b.c.d/x, iptables will ignore the host subnet bits
			if err := setINC(thisV4.String(), otherV4.String(), enable); err != nil {
				return err
			}
		}

		if thisV6 != nil && otherV6 != nil && !types.CompareIPNet(thisV6, otherV6) {
			if err := setINC(thisV6.String(), otherV6.String(), enable); err != nil {
				return err
			}
		}
	}

	return nil
}

// Checks whether this network's configuration for the network with this id conflicts with any of the passed networks
func (c *networkConfiguration) conflictsWithNetworks(id string, others []*bridgeNetwork) error {
	for _, nw := range others {

		nw.Lock()
		nwID := nw.id
		nwConfig := nw.config
		nwBridge := nw.bridge
		nw.Unlock()

		if nwID == id {
			continue
		}
		// Verify the name (which may have been set by newInterface()) does not conflict with
		// existing bridge interfaces. Ironically the system chosen name gets stored in the config...
		// Basically we are checking if the two original configs were both empty.
		if nwConfig.BridgeName == c.BridgeName {
			return types.ForbiddenErrorf("conflicts with network %s (%s) by bridge name", nwID, nwConfig.BridgeName)
		}
		// If this network config specifies the AddressIPv4, we need
		// to make sure it does not conflict with any previously allocated
		// bridges. This could not be completely caught by the config conflict
		// check, because networks which config does not specify the AddressIPv4
		// get their address and subnet selected by the driver (see electBridgeIPv4())
		if c.AddressIPv4 != nil {
			if nwBridge.bridgeIPv4.Contains(c.AddressIPv4.IP) ||
				c.AddressIPv4.Contains(nwBridge.bridgeIPv4.IP) {
				return types.ForbiddenErrorf("conflicts with network %s (%s) by ip network", nwID, nwConfig.BridgeName)
			}
		}
	}

	return nil
}

func (d *driver) Config(option map[string]interface{}) error {
	var config *configuration
	var err error

	d.Lock()
	defer d.Unlock()

	if d.config != nil {
		return &ErrConfigExists{}
	}

	genericData, ok := option[netlabel.GenericData]
	if ok && genericData != nil {
		switch opt := genericData.(type) {
		case options.Generic:
			opaqueConfig, err := options.GenerateFromModel(opt, &configuration{})
			if err != nil {
				return err
			}
			config = opaqueConfig.(*configuration)
		case *configuration:
			config = opt
		default:
			return &ErrInvalidDriverConfig{}
		}

		d.config = config
	} else {
		config = &configuration{}
		d.config = config
	}

	if config.EnableIPForwarding {
		err = setupIPForwarding()
		if err != nil {
			return err
		}
	}

	if config.EnableIPTables {
		d.natChain, d.filterChain, err = setupIPChains(config)
		return err
	}

	return nil
}

func (d *driver) getNetwork(id string) (*bridgeNetwork, error) {
	d.Lock()
	defer d.Unlock()

	if id == "" {
		return nil, types.BadRequestErrorf("invalid network id: %s", id)
	}

	if nw, ok := d.networks[id]; ok {
		return nw, nil
	}

	return nil, types.NotFoundErrorf("network not found: %s", id)
}

func parseNetworkGenericOptions(data interface{}) (*networkConfiguration, error) {
	var (
		err    error
		config *networkConfiguration
	)

	switch opt := data.(type) {
	case *networkConfiguration:
		config = opt
	case map[string]interface{}:
		config = &networkConfiguration{
			EnableICC:          true,
			EnableIPMasquerade: true,
		}
		err = config.fromMap(opt)
	case options.Generic:
		var opaqueConfig interface{}
		if opaqueConfig, err = options.GenerateFromModel(opt, config); err == nil {
			config = opaqueConfig.(*networkConfiguration)
		}
	default:
		err = types.BadRequestErrorf("do not recognize network configuration format: %T", opt)
	}

	return config, err
}

func parseNetworkOptions(option options.Generic) (*networkConfiguration, error) {
	var err error
	config := &networkConfiguration{}

	// Parse generic label first, config will be re-assigned
	if genData, ok := option[netlabel.GenericData]; ok && genData != nil {
		if config, err = parseNetworkGenericOptions(genData); err != nil {
			return nil, err
		}
	}

	// Process well-known labels next
	if _, ok := option[netlabel.EnableIPv6]; ok {
		config.EnableIPv6 = option[netlabel.EnableIPv6].(bool)
	}

	// Finally validate the configuration
	if err = config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// Returns the non link-local IPv6 subnet for the containers attached to this bridge if found, nil otherwise
func getV6Network(config *networkConfiguration, i *bridgeInterface) *net.IPNet {
	if config.FixedCIDRv6 != nil {
		return config.FixedCIDRv6
	}

	if i.bridgeIPv6 != nil && i.bridgeIPv6.IP != nil && !i.bridgeIPv6.IP.IsLinkLocalUnicast() {
		return i.bridgeIPv6
	}

	return nil
}

// Return a slice of networks over which caller can iterate safely
func (d *driver) getNetworks() []*bridgeNetwork {
	d.Lock()
	defer d.Unlock()

	ls := make([]*bridgeNetwork, 0, len(d.networks))
	for _, nw := range d.networks {
		ls = append(ls, nw)
	}
	return ls
}

// Create a new network using bridge plugin
func (d *driver) CreateNetwork(id string, option map[string]interface{}) error {
	var err error

	defer osl.InitOSContext()()

	// Sanity checks
	d.Lock()
	if _, ok := d.networks[id]; ok {
		d.Unlock()
		return types.ForbiddenErrorf("network %s exists", id)
	}
	d.Unlock()

	// Parse and validate the config. It should not conflict with existing networks' config
	config, err := parseNetworkOptions(option)
	if err != nil {
		return err
	}
	networkList := d.getNetworks()
	for _, nw := range networkList {
		nw.Lock()
		nwConfig := nw.config
		nw.Unlock()
		if nwConfig.Conflicts(config) {
			return types.ForbiddenErrorf("conflicts with network %s (%s)", nw.id, nw.config.BridgeName)
		}
	}

	// Create and set network handler in driver
	network := &bridgeNetwork{
		id:         id,
		endpoints:  make(map[string]*bridgeEndpoint),
		config:     config,
		portMapper: portmapper.New(),
		driver:     d,
	}

	d.Lock()
	d.networks[id] = network
	d.Unlock()

	// On failure make sure to reset driver network handler to nil
	defer func() {
		if err != nil {
			d.Lock()
			delete(d.networks, id)
			d.Unlock()
		}
	}()

	// Create or retrieve the bridge L3 interface
	bridgeIface := newInterface(config)
	network.bridge = bridgeIface

	// Verify the network configuration does not conflict with previously installed
	// networks. This step is needed now because driver might have now set the bridge
	// name on this config struct. And because we need to check for possible address
	// conflicts, so we need to check against operationa lnetworks.
	if err = config.conflictsWithNetworks(id, networkList); err != nil {
		return err
	}

	setupNetworkIsolationRules := func(config *networkConfiguration, i *bridgeInterface) error {
		if err := network.isolateNetwork(networkList, true); err != nil {
			if err := network.isolateNetwork(networkList, false); err != nil {
				logrus.Warnf("Failed on removing the inter-network iptables rules on cleanup: %v", err)
			}
			return err
		}
		return nil
	}

	// Prepare the bridge setup configuration
	bridgeSetup := newBridgeSetup(config, bridgeIface)

	// If the bridge interface doesn't exist, we need to start the setup steps
	// by creating a new device and assigning it an IPv4 address.
	bridgeAlreadyExists := bridgeIface.exists()
	if !bridgeAlreadyExists {
		bridgeSetup.queueStep(setupDevice)
	}

	// Even if a bridge exists try to setup IPv4.
	bridgeSetup.queueStep(setupBridgeIPv4)

	enableIPv6Forwarding := false
	if d.config != nil && d.config.EnableIPForwarding && config.FixedCIDRv6 != nil {
		enableIPv6Forwarding = true
	}

	// Conditionally queue setup steps depending on configuration values.
	for _, step := range []struct {
		Condition bool
		Fn        setupStep
	}{
		// Enable IPv6 on the bridge if required. We do this even for a
		// previously  existing bridge, as it may be here from a previous
		// installation where IPv6 wasn't supported yet and needs to be
		// assigned an IPv6 link-local address.
		{config.EnableIPv6, setupBridgeIPv6},

		// We ensure that the bridge has the expectedIPv4 and IPv6 addresses in
		// the case of a previously existing device.
		{bridgeAlreadyExists, setupVerifyAndReconcile},

		// Setup the bridge to allocate containers IPv4 addresses in the
		// specified subnet.
		{config.FixedCIDR != nil, setupFixedCIDRv4},

		// Setup the bridge to allocate containers global IPv6 addresses in the
		// specified subnet.
		{config.FixedCIDRv6 != nil, setupFixedCIDRv6},

		// Enable IPv6 Forwarding
		{enableIPv6Forwarding, setupIPv6Forwarding},

		// Setup Loopback Adresses Routing
		{!d.config.EnableUserlandProxy, setupLoopbackAdressesRouting},

		// Setup IPTables.
		{d.config.EnableIPTables, network.setupIPTables},

		//We want to track firewalld configuration so that
		//if it is started/reloaded, the rules can be applied correctly
		{d.config.EnableIPTables, network.setupFirewalld},

		// Setup DefaultGatewayIPv4
		{config.DefaultGatewayIPv4 != nil, setupGatewayIPv4},

		// Setup DefaultGatewayIPv6
		{config.DefaultGatewayIPv6 != nil, setupGatewayIPv6},

		// Add inter-network communication rules.
		{d.config.EnableIPTables, setupNetworkIsolationRules},

		//Configure bridge networking filtering if ICC is off and IP tables are enabled
		{!config.EnableICC && d.config.EnableIPTables, setupBridgeNetFiltering},
	} {
		if step.Condition {
			bridgeSetup.queueStep(step.Fn)
		}
	}

	// Block bridge IP from being allocated.
	bridgeSetup.queueStep(allocateBridgeIP)
	// Apply the prepared list of steps, and abort at the first error.
	bridgeSetup.queueStep(setupDeviceUp)
	if err = bridgeSetup.apply(); err != nil {
		return err
	}

	return nil
}

func (d *driver) DeleteNetwork(nid string) error {
	var err error

	defer osl.InitOSContext()()

	// Get network handler and remove it from driver
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()

	if !ok {
		return types.InternalMaskableErrorf("network %s does not exist", nid)
	}

	n.Lock()
	config := n.config
	n.Unlock()

	if config.BridgeName == DefaultBridgeName {
		return types.ForbiddenErrorf("default network of type \"%s\" cannot be deleted", networkType)
	}

	d.Lock()
	delete(d.networks, nid)
	d.Unlock()

	// On failure set network handler back in driver, but
	// only if is not already taken over by some other thread
	defer func() {
		if err != nil {
			d.Lock()
			if _, ok := d.networks[nid]; !ok {
				d.networks[nid] = n
			}
			d.Unlock()
		}
	}()

	// Sanity check
	if n == nil {
		err = driverapi.ErrNoNetwork(nid)
		return err
	}

	// Cannot remove network if endpoints are still present
	if len(n.endpoints) != 0 {
		err = ActiveEndpointsError(n.id)
		return err
	}

	// In case of failures after this point, restore the network isolation rules
	nwList := d.getNetworks()
	defer func() {
		if err != nil {
			if err := n.isolateNetwork(nwList, true); err != nil {
				logrus.Warnf("Failed on restoring the inter-network iptables rules on cleanup: %v", err)
			}
		}
	}()

	// Remove inter-network communication rules.
	err = n.isolateNetwork(nwList, false)
	if err != nil {
		return err
	}

	// Programming
	err = netlink.LinkDel(n.bridge.Link)

	// Release ip addresses (ignore errors)
	if config.FixedCIDR == nil || config.FixedCIDR.Contains(config.DefaultGatewayIPv4) {
		if e := ipAllocator.ReleaseIP(n.bridge.bridgeIPv4, n.bridge.gatewayIPv4); e != nil {
			logrus.Warnf("Failed to release default gateway address %s: %v", n.bridge.gatewayIPv4.String(), e)
		}
	}
	if config.FixedCIDR == nil || config.FixedCIDR.Contains(n.bridge.bridgeIPv4.IP) {
		if e := ipAllocator.ReleaseIP(n.bridge.bridgeIPv4, n.bridge.bridgeIPv4.IP); e != nil {
			logrus.Warnf("Failed to release bridge IP %s: %v", n.bridge.bridgeIPv4.IP.String(), e)
		}
	}

	return err
}

func addToBridge(ifaceName, bridgeName string) error {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("could not find interface %s: %v", ifaceName, err)
	}
	if err = netlink.LinkSetMaster(link,
		&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeName}}); err != nil {
		logrus.Debugf("Failed to add %s to bridge via netlink.Trying ioctl: %v", ifaceName, err)
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return fmt.Errorf("could not find network interface %s: %v", ifaceName, err)
		}

		master, err := net.InterfaceByName(bridgeName)
		if err != nil {
			return fmt.Errorf("could not find bridge %s: %v", bridgeName, err)
		}

		return ioctlAddToBridge(iface, master)
	}
	return nil
}

func setHairpinMode(link netlink.Link, enable bool) error {
	err := netlink.LinkSetHairpin(link, enable)
	if err != nil && err != syscall.EINVAL {
		// If error is not EINVAL something else went wrong, bail out right away
		return fmt.Errorf("unable to set hairpin mode on %s via netlink: %v",
			link.Attrs().Name, err)
	}

	// Hairpin mode successfully set up
	if err == nil {
		return nil
	}

	// The netlink method failed with EINVAL which is probably because of an older
	// kernel. Try one more time via the sysfs method.
	path := filepath.Join("/sys/class/net", link.Attrs().Name, "brport/hairpin_mode")

	var val []byte
	if enable {
		val = []byte{'1', '\n'}
	} else {
		val = []byte{'0', '\n'}
	}

	if err := ioutil.WriteFile(path, val, 0644); err != nil {
		return fmt.Errorf("unable to set hairpin mode on %s via sysfs: %v", link.Attrs().Name, err)
	}

	return nil
}

func (d *driver) CreateEndpoint(nid, eid string, epInfo driverapi.EndpointInfo, epOptions map[string]interface{}) error {
	var (
		ipv6Addr *net.IPNet
		err      error
	)

	defer osl.InitOSContext()()

	if epInfo == nil {
		return errors.New("invalid endpoint info passed")
	}

	if len(epInfo.Interfaces()) != 0 {
		return errors.New("non empty interface list passed to bridge(local) driver")
	}

	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	dconfig := d.config
	d.Unlock()

	if !ok {
		return types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}

	// Endpoint with that id exists either on desired or other sandbox
	if ep != nil {
		return driverapi.ErrEndpointExists(eid)
	}

	// Try to convert the options to endpoint configuration
	epConfig, err := parseEndpointOptions(epOptions)
	if err != nil {
		return err
	}

	// Create and add the endpoint
	n.Lock()
	endpoint := &bridgeEndpoint{id: eid, config: epConfig}
	n.endpoints[eid] = endpoint
	n.Unlock()

	// On failure make sure to remove the endpoint
	defer func() {
		if err != nil {
			n.Lock()
			delete(n.endpoints, eid)
			n.Unlock()
		}
	}()

	// Generate a name for what will be the host side pipe interface
	hostIfName, err := netutils.GenerateIfaceName(vethPrefix, vethLen)
	if err != nil {
		return err
	}

	// Generate a name for what will be the sandbox side pipe interface
	containerIfName, err := netutils.GenerateIfaceName(vethPrefix, vethLen)
	if err != nil {
		return err
	}

	// Generate and add the interface pipe host <-> sandbox
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostIfName, TxQLen: 0},
		PeerName:  containerIfName}
	if err = netlink.LinkAdd(veth); err != nil {
		return types.InternalErrorf("failed to add the host (%s) <=> sandbox (%s) pair interfaces: %v", hostIfName, containerIfName, err)
	}

	// Get the host side pipe interface handler
	host, err := netlink.LinkByName(hostIfName)
	if err != nil {
		return types.InternalErrorf("failed to find host side interface %s: %v", hostIfName, err)
	}
	defer func() {
		if err != nil {
			netlink.LinkDel(host)
		}
	}()

	// Get the sandbox side pipe interface handler
	sbox, err := netlink.LinkByName(containerIfName)
	if err != nil {
		return types.InternalErrorf("failed to find sandbox side interface %s: %v", containerIfName, err)
	}
	defer func() {
		if err != nil {
			netlink.LinkDel(sbox)
		}
	}()

	n.Lock()
	config := n.config
	n.Unlock()

	// Add bridge inherited attributes to pipe interfaces
	if config.Mtu != 0 {
		err = netlink.LinkSetMTU(host, config.Mtu)
		if err != nil {
			return types.InternalErrorf("failed to set MTU on host interface %s: %v", hostIfName, err)
		}
		err = netlink.LinkSetMTU(sbox, config.Mtu)
		if err != nil {
			return types.InternalErrorf("failed to set MTU on sandbox interface %s: %v", containerIfName, err)
		}
	}

	// Attach host side pipe interface into the bridge
	if err = addToBridge(hostIfName, config.BridgeName); err != nil {
		return fmt.Errorf("adding interface %s to bridge %s failed: %v", hostIfName, config.BridgeName, err)
	}

	if !dconfig.EnableUserlandProxy {
		err = setHairpinMode(host, true)
		if err != nil {
			return err
		}
	}

	// v4 address for the sandbox side pipe interface
	ip4, err := ipAllocator.RequestIP(n.bridge.bridgeIPv4, nil)
	if err != nil {
		return err
	}
	ipv4Addr := &net.IPNet{IP: ip4, Mask: n.bridge.bridgeIPv4.Mask}

	// Down the interface before configuring mac address.
	if err = netlink.LinkSetDown(sbox); err != nil {
		return fmt.Errorf("could not set link down for container interface %s: %v", containerIfName, err)
	}

	// Set the sbox's MAC. If specified, use the one configured by user, otherwise generate one based on IP.
	mac := electMacAddress(epConfig, ip4)
	err = netlink.LinkSetHardwareAddr(sbox, mac)
	if err != nil {
		return fmt.Errorf("could not set mac address for container interface %s: %v", containerIfName, err)
	}
	endpoint.macAddress = mac

	// Up the host interface after finishing all netlink configuration
	if err = netlink.LinkSetUp(host); err != nil {
		return fmt.Errorf("could not set link up for host interface %s: %v", hostIfName, err)
	}

	// v6 address for the sandbox side pipe interface
	ipv6Addr = &net.IPNet{}
	if config.EnableIPv6 {
		var ip6 net.IP

		network := n.bridge.bridgeIPv6
		if config.FixedCIDRv6 != nil {
			network = config.FixedCIDRv6
		}

		ones, _ := network.Mask.Size()
		if ones <= 80 {
			ip6 = make(net.IP, len(network.IP))
			copy(ip6, network.IP)
			for i, h := range mac {
				ip6[i+10] = h
			}
		}

		ip6, err := ipAllocator.RequestIP(network, ip6)
		if err != nil {
			return err
		}

		ipv6Addr = &net.IPNet{IP: ip6, Mask: network.Mask}
	}

	// Create the sandbox side pipe interface
	endpoint.srcName = containerIfName
	endpoint.addr = ipv4Addr

	if config.EnableIPv6 {
		endpoint.addrv6 = ipv6Addr
	}

	err = epInfo.AddInterface(ifaceID, endpoint.macAddress, *ipv4Addr, *ipv6Addr)
	if err != nil {
		return err
	}

	// Program any required port mapping and store them in the endpoint
	endpoint.portMapping, err = n.allocatePorts(epConfig, endpoint, config.DefaultBindingIP, d.config.EnableUserlandProxy)
	if err != nil {
		return err
	}

	return nil
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	var err error

	defer osl.InitOSContext()()

	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()

	if !ok {
		return types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity Check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check endpoint id and if an endpoint is actually there
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}
	if ep == nil {
		return EndpointNotFoundError(eid)
	}

	// Remove it
	n.Lock()
	delete(n.endpoints, eid)
	n.Unlock()

	// On failure make sure to set back ep in n.endpoints, but only
	// if it hasn't been taken over already by some other thread.
	defer func() {
		if err != nil {
			n.Lock()
			if _, ok := n.endpoints[eid]; !ok {
				n.endpoints[eid] = ep
			}
			n.Unlock()
		}
	}()

	// Remove port mappings. Do not stop endpoint delete on unmap failure
	n.releasePorts(ep)

	// Release the v4 address allocated to this endpoint's sandbox interface
	err = ipAllocator.ReleaseIP(n.bridge.bridgeIPv4, ep.addr.IP)
	if err != nil {
		return err
	}

	n.Lock()
	config := n.config
	n.Unlock()

	// Release the v6 address allocated to this endpoint's sandbox interface
	if config.EnableIPv6 {
		network := n.bridge.bridgeIPv6
		if config.FixedCIDRv6 != nil {
			network = config.FixedCIDRv6
		}
		err := ipAllocator.ReleaseIP(network, ep.addrv6.IP)
		if err != nil {
			return err
		}
	}

	// Try removal of link. Discard error: link pair might have
	// already been deleted by sandbox delete. Make sure defer
	// does not see this error either.
	if link, err := netlink.LinkByName(ep.srcName); err == nil {
		netlink.LinkDel(link)
	}

	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]interface{}, error) {
	// Get the network handler and make sure it exists
	d.Lock()
	n, ok := d.networks[nid]
	d.Unlock()
	if !ok {
		return nil, types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return nil, driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return nil, InvalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return nil, err
	}
	if ep == nil {
		return nil, driverapi.ErrNoEndpoint(eid)
	}

	m := make(map[string]interface{})

	if ep.config.ExposedPorts != nil {
		// Return a copy of the config data
		epc := make([]types.TransportPort, 0, len(ep.config.ExposedPorts))
		for _, tp := range ep.config.ExposedPorts {
			epc = append(epc, tp.GetCopy())
		}
		m[netlabel.ExposedPorts] = epc
	}

	if ep.portMapping != nil {
		// Return a copy of the operational data
		pmc := make([]types.PortBinding, 0, len(ep.portMapping))
		for _, pm := range ep.portMapping {
			pmc = append(pmc, pm.GetCopy())
		}
		m[netlabel.PortMap] = pmc
	}

	if len(ep.macAddress) != 0 {
		m[netlabel.MacAddress] = ep.macAddress
	}

	return m, nil
}

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, options map[string]interface{}) error {
	defer osl.InitOSContext()()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return EndpointNotFoundError(eid)
	}

	for _, iNames := range jinfo.InterfaceNames() {
		// Make sure to set names on the correct interface ID.
		if iNames.ID() == ifaceID {
			err = iNames.SetNames(endpoint.srcName, containerVethPrefix)
			if err != nil {
				return err
			}
		}
	}

	err = jinfo.SetGateway(network.bridge.gatewayIPv4)
	if err != nil {
		return err
	}

	err = jinfo.SetGatewayIPv6(network.bridge.gatewayIPv6)
	if err != nil {
		return err
	}

	if !network.config.EnableICC {
		return d.link(network, endpoint, options, true)
	}

	return nil
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
	defer osl.InitOSContext()()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return EndpointNotFoundError(eid)
	}

	if !network.config.EnableICC {
		return d.link(network, endpoint, nil, false)
	}

	return nil
}

func (d *driver) link(network *bridgeNetwork, endpoint *bridgeEndpoint, options map[string]interface{}, enable bool) error {
	var (
		cc  *containerConfiguration
		err error
	)

	if enable {
		cc, err = parseContainerOptions(options)
		if err != nil {
			return err
		}
	} else {
		cc = endpoint.containerConfig
	}

	if cc == nil {
		return nil
	}

	if endpoint.config != nil && endpoint.config.ExposedPorts != nil {
		for _, p := range cc.ParentEndpoints {
			var parentEndpoint *bridgeEndpoint
			parentEndpoint, err = network.getEndpoint(p)
			if err != nil {
				return err
			}
			if parentEndpoint == nil {
				err = InvalidEndpointIDError(p)
				return err
			}

			l := newLink(parentEndpoint.addr.IP.String(),
				endpoint.addr.IP.String(),
				endpoint.config.ExposedPorts, network.config.BridgeName)
			if enable {
				err = l.Enable()
				if err != nil {
					return err
				}
				defer func() {
					if err != nil {
						l.Disable()
					}
				}()
			} else {
				l.Disable()
			}
		}
	}

	for _, c := range cc.ChildEndpoints {
		var childEndpoint *bridgeEndpoint
		childEndpoint, err = network.getEndpoint(c)
		if err != nil {
			return err
		}
		if childEndpoint == nil {
			err = InvalidEndpointIDError(c)
			return err
		}
		if childEndpoint.config == nil || childEndpoint.config.ExposedPorts == nil {
			continue
		}

		l := newLink(endpoint.addr.IP.String(),
			childEndpoint.addr.IP.String(),
			childEndpoint.config.ExposedPorts, network.config.BridgeName)
		if enable {
			err = l.Enable()
			if err != nil {
				return err
			}
			defer func() {
				if err != nil {
					l.Disable()
				}
			}()
		} else {
			l.Disable()
		}
	}

	if enable {
		endpoint.containerConfig = cc
	}

	return nil
}

func (d *driver) Type() string {
	return networkType
}

func parseEndpointOptions(epOptions map[string]interface{}) (*endpointConfiguration, error) {
	if epOptions == nil {
		return nil, nil
	}

	ec := &endpointConfiguration{}

	if opt, ok := epOptions[netlabel.MacAddress]; ok {
		if mac, ok := opt.(net.HardwareAddr); ok {
			ec.MacAddress = mac
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	if opt, ok := epOptions[netlabel.PortMap]; ok {
		if bs, ok := opt.([]types.PortBinding); ok {
			ec.PortBindings = bs
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	if opt, ok := epOptions[netlabel.ExposedPorts]; ok {
		if ports, ok := opt.([]types.TransportPort); ok {
			ec.ExposedPorts = ports
		} else {
			return nil, &ErrInvalidEndpointConfig{}
		}
	}

	return ec, nil
}

func parseContainerOptions(cOptions map[string]interface{}) (*containerConfiguration, error) {
	if cOptions == nil {
		return nil, nil
	}
	genericData := cOptions[netlabel.GenericData]
	if genericData == nil {
		return nil, nil
	}
	switch opt := genericData.(type) {
	case options.Generic:
		opaqueConfig, err := options.GenerateFromModel(opt, &containerConfiguration{})
		if err != nil {
			return nil, err
		}
		return opaqueConfig.(*containerConfiguration), nil
	case *containerConfiguration:
		return opt, nil
	default:
		return nil, nil
	}
}

func electMacAddress(epConfig *endpointConfiguration, ip net.IP) net.HardwareAddr {
	if epConfig != nil && epConfig.MacAddress != nil {
		return epConfig.MacAddress
	}
	return netutils.GenerateMACFromIP(ip)
}
