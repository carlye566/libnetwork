package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"testing"

	"github.com/docker/docker/pkg/reexec"
	"github.com/docker/libnetwork"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/options"
	"github.com/docker/libnetwork/osl"
	"github.com/docker/libnetwork/types"
)

const (
	bridgeNetType = "bridge"
	bridgeName    = "docker0"
)

func getEmptyGenericOption() map[string]interface{} {
	genericOption := make(map[string]interface{})
	genericOption[netlabel.GenericData] = options.Generic{}
	return genericOption
}

func i2s(i interface{}) string {
	s, ok := i.(string)
	if !ok {
		panic(fmt.Sprintf("Failed i2s for %v", i))
	}
	return s
}

func i2e(i interface{}) *endpointResource {
	s, ok := i.(*endpointResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2e for %v", i))
	}
	return s
}

func i2eL(i interface{}) []*endpointResource {
	s, ok := i.([]*endpointResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2eL for %v", i))
	}
	return s
}

func i2n(i interface{}) *networkResource {
	s, ok := i.(*networkResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2n for %v", i))
	}
	return s
}

func i2nL(i interface{}) []*networkResource {
	s, ok := i.([]*networkResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2nL for %v", i))
	}
	return s
}

func i2sb(i interface{}) *sandboxResource {
	s, ok := i.(*sandboxResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2sb for %v", i))
	}
	return s
}

func i2sbL(i interface{}) []*sandboxResource {
	s, ok := i.([]*sandboxResource)
	if !ok {
		panic(fmt.Sprintf("Failed i2sbL for %v", i))
	}
	return s
}

func createTestNetwork(t *testing.T, network string) (libnetwork.NetworkController, libnetwork.Network) {
	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}

	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	netOption := options.Generic{
		netlabel.GenericData: options.Generic{
			"BridgeName":            network,
			"AllowNonDefaultBridge": true,
		},
	}
	netGeneric := libnetwork.NetworkOptionGeneric(netOption)
	nw, err := c.NewNetwork(bridgeNetType, network, netGeneric)
	if err != nil {
		t.Fatal(err)
	}

	return c, nw
}

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

func TestSandboxOptionParser(t *testing.T) {
	hn := "host1"
	dn := "docker.com"
	hp := "/etc/hosts"
	rc := "/etc/resolv.conf"
	dnss := []string{"8.8.8.8", "172.28.34.5"}
	ehs := []extraHost{extraHost{Name: "extra1", Address: "172.28.9.1"}, extraHost{Name: "extra2", Address: "172.28.9.2"}}

	sb := sandboxCreate{
		HostName:          hn,
		DomainName:        dn,
		HostsPath:         hp,
		ResolvConfPath:    rc,
		DNS:               dnss,
		ExtraHosts:        ehs,
		UseDefaultSandbox: true,
	}

	if len(sb.parseOptions()) != 9 {
		t.Fatalf("Failed to generate all libnetwork.SandboxOption methods")
	}
}

func TestJson(t *testing.T) {
	nc := networkCreate{NetworkType: bridgeNetType}
	b, err := json.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}

	var ncp networkCreate
	err = json.Unmarshal(b, &ncp)
	if err != nil {
		t.Fatal(err)
	}

	if nc.NetworkType != ncp.NetworkType {
		t.Fatalf("Incorrect networkCreate after json encoding/deconding: %v", ncp)
	}

	jl := endpointJoin{SandboxID: "abcdef456789"}
	b, err = json.Marshal(jl)
	if err != nil {
		t.Fatal(err)
	}

	var jld endpointJoin
	err = json.Unmarshal(b, &jld)
	if err != nil {
		t.Fatal(err)
	}

	if jl.SandboxID != jld.SandboxID {
		t.Fatalf("Incorrect endpointJoin after json encoding/deconding: %v", jld)
	}
}

func TestCreateDeleteNetwork(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	badBody, err := json.Marshal("bad body")
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]string)
	_, errRsp := procCreateNetwork(c, nil, badBody)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected StatusBadRequest status code, got: %v", errRsp)
	}

	incompleteBody, err := json.Marshal(networkCreate{})
	if err != nil {
		t.Fatal(err)
	}

	_, errRsp = procCreateNetwork(c, vars, incompleteBody)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected StatusBadRequest status code, got: %v", errRsp)
	}

	ops := options.Generic{
		netlabel.EnableIPv6: true,
		netlabel.GenericData: map[string]string{
			"BridgeName":            "abc",
			"AllowNonDefaultBridge": "true",
			"FixedCIDRv6":           "fe80::1/64",
			"AddressIP":             "172.28.30.254/24",
		},
	}
	nc := networkCreate{Name: "network_1", NetworkType: bridgeNetType, Options: ops}
	goodBody, err := json.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}

	_, errRsp = procCreateNetwork(c, vars, goodBody)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	vars[urlNwName] = ""
	_, errRsp = procDeleteNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected to fail but succeeded")
	}

	vars[urlNwName] = "abc"
	_, errRsp = procDeleteNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected to fail but succeeded")
	}

	vars[urlNwName] = "network_1"
	_, errRsp = procDeleteNetwork(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
}

func TestGetNetworksAndEndpoints(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	ops := options.Generic{
		netlabel.GenericData: map[string]string{
			"BridgeName":            "api_test_nw",
			"AllowNonDefaultBridge": "true",
		},
	}

	nc := networkCreate{Name: "sh", NetworkType: bridgeNetType, Options: ops}
	body, err := json.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]string)
	inid, errRsp := procCreateNetwork(c, vars, body)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	nid, ok := inid.(string)
	if !ok {
		t.FailNow()
	}

	ec1 := endpointCreate{
		Name: "ep1",
		ExposedPorts: []types.TransportPort{
			types.TransportPort{Proto: types.TCP, Port: uint16(5000)},
			types.TransportPort{Proto: types.UDP, Port: uint16(400)},
			types.TransportPort{Proto: types.TCP, Port: uint16(600)},
		},
		PortMapping: []types.PortBinding{
			types.PortBinding{Proto: types.TCP, Port: uint16(230), HostPort: uint16(23000)},
			types.PortBinding{Proto: types.UDP, Port: uint16(200), HostPort: uint16(22000)},
			types.PortBinding{Proto: types.TCP, Port: uint16(120), HostPort: uint16(12000)},
		},
	}
	b1, err := json.Marshal(ec1)
	if err != nil {
		t.Fatal(err)
	}
	ec2 := endpointCreate{Name: "ep2"}
	b2, err := json.Marshal(ec2)
	if err != nil {
		t.Fatal(err)
	}

	vars[urlNwName] = "sh"
	vars[urlEpName] = "ep1"
	ieid1, errRsp := procCreateEndpoint(c, vars, b1)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	eid1 := i2s(ieid1)
	vars[urlEpName] = "ep2"
	ieid2, errRsp := procCreateEndpoint(c, vars, b2)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	eid2 := i2s(ieid2)

	vars[urlNwName] = ""
	vars[urlEpName] = "ep1"
	_, errRsp = procGetEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure but succeeded: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected to fail with http.StatusBadRequest, but got: %d", errRsp.StatusCode)
	}

	vars = make(map[string]string)
	vars[urlNwName] = "sh"
	vars[urlEpID] = ""
	_, errRsp = procGetEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure but succeeded: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected to fail with http.StatusBadRequest, but got: %d", errRsp.StatusCode)
	}

	vars = make(map[string]string)
	vars[urlNwID] = ""
	vars[urlEpID] = eid1
	_, errRsp = procGetEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure but succeeded: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected to fail with http.StatusBadRequest, but got: %d", errRsp.StatusCode)
	}

	// nw by name and ep by id
	vars[urlNwName] = "sh"
	i1, errRsp := procGetEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	// nw by name and ep by name
	delete(vars, urlEpID)
	vars[urlEpName] = "ep1"
	i2, errRsp := procGetEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	// nw by id and ep by name
	delete(vars, urlNwName)
	vars[urlNwID] = nid
	i3, errRsp := procGetEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	// nw by id and ep by id
	delete(vars, urlEpName)
	vars[urlEpID] = eid1
	i4, errRsp := procGetEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	id1 := i2e(i1).ID
	if id1 != i2e(i2).ID || id1 != i2e(i3).ID || id1 != i2e(i4).ID {
		t.Fatalf("Endpoints retireved via different query parameters differ: %v, %v, %v, %v", i1, i2, i3, i4)
	}

	vars[urlNwName] = ""
	_, errRsp = procGetEndpoints(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	delete(vars, urlNwName)
	vars[urlNwID] = "fakeID"
	_, errRsp = procGetEndpoints(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlNwID] = nid
	_, errRsp = procGetEndpoints(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	vars[urlNwName] = "sh"
	iepList, errRsp := procGetEndpoints(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	epList := i2eL(iepList)
	if len(epList) != 2 {
		t.Fatalf("Did not return the expected number (2) of endpoint resources: %d", len(epList))
	}
	if "sh" != epList[0].Network || "sh" != epList[1].Network {
		t.Fatalf("Did not find expected network name in endpoint resources")
	}

	vars = make(map[string]string)
	vars[urlNwName] = ""
	_, errRsp = procGetNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Exepected failure, got: %v", errRsp)
	}
	vars[urlNwName] = "shhhhh"
	_, errRsp = procGetNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Exepected failure, got: %v", errRsp)
	}
	vars[urlNwName] = "sh"
	inr1, errRsp := procGetNetwork(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	nr1 := i2n(inr1)

	delete(vars, urlNwName)
	vars[urlNwID] = "cacca"
	_, errRsp = procGetNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	vars[urlNwID] = nid
	inr2, errRsp := procGetNetwork(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("procgetNetworkByName() != procgetNetworkById(), %v vs %v", inr1, inr2)
	}
	nr2 := i2n(inr2)
	if nr1.Name != nr2.Name || nr1.Type != nr2.Type || nr1.ID != nr2.ID || len(nr1.Endpoints) != len(nr2.Endpoints) {
		t.Fatalf("Get by name and Get failure: %v", errRsp)
	}

	if len(nr1.Endpoints) != 2 {
		t.Fatalf("Did not find the expected number (2) of endpoint resources in the network resource: %d", len(nr1.Endpoints))
	}
	for _, er := range nr1.Endpoints {
		if er.ID != eid1 && er.ID != eid2 {
			t.Fatalf("Did not find the expected endpoint resources in the network resource: %v", nr1.Endpoints)
		}
	}

	iList, errRsp := procGetNetworks(c, nil, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	netList := i2nL(iList)
	if len(netList) != 1 {
		t.Fatalf("Did not return the expected number of network resources")
	}
	if nid != netList[0].ID {
		t.Fatalf("Did not find expected network %s: %v", nid, netList)
	}

	_, errRsp = procDeleteNetwork(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Exepected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "ep1"
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	delete(vars, urlEpName)
	iepList, errRsp = procGetEndpoints(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	epList = i2eL(iepList)
	if len(epList) != 1 {
		t.Fatalf("Did not return the expected number (1) of endpoint resources: %d", len(epList))
	}

	vars[urlEpName] = "ep2"
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	iepList, errRsp = procGetEndpoints(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	epList = i2eL(iepList)
	if len(epList) != 0 {
		t.Fatalf("Did not return the expected number (0) of endpoint resources: %d", len(epList))
	}

	_, errRsp = procDeleteNetwork(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	iList, errRsp = procGetNetworks(c, nil, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	netList = i2nL(iList)
	if len(netList) != 0 {
		t.Fatalf("Did not return the expected number of network resources")
	}
}

func TestProcGetServices(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}

	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create 2 networks
	netName1 := "production"
	netOption := options.Generic{
		netlabel.GenericData: options.Generic{
			"BridgeName":            netName1,
			"AllowNonDefaultBridge": true,
		},
	}
	nw1, err := c.NewNetwork(bridgeNetType, netName1, libnetwork.NetworkOptionGeneric(netOption))
	if err != nil {
		t.Fatal(err)
	}

	netName2 := "work-dev"
	netOption = options.Generic{
		netlabel.GenericData: options.Generic{
			"BridgeName":            netName2,
			"AllowNonDefaultBridge": true,
		},
	}
	nw2, err := c.NewNetwork(bridgeNetType, netName2, libnetwork.NetworkOptionGeneric(netOption))
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]string)
	li, errRsp := procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list := i2eL(li)
	if len(list) != 0 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	// Add a couple of services on one network and one on the other network
	ep11, err := nw1.CreateEndpoint("db-prod")
	if err != nil {
		t.Fatal(err)
	}
	ep12, err := nw1.CreateEndpoint("web-prod")
	if err != nil {
		t.Fatal(err)
	}
	ep21, err := nw2.CreateEndpoint("db-dev")
	if err != nil {
		t.Fatal(err)
	}

	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 3 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	// Filter by network
	vars[urlNwName] = netName1
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 2 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	vars[urlNwName] = netName2
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 1 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	vars[urlNwName] = "unknown-network"
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 0 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	// Query by name
	delete(vars, urlNwName)
	vars[urlEpName] = "db-prod"
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 1 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	vars[urlEpName] = "no-service"
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 0 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	// Query by id or partial id
	delete(vars, urlEpName)
	vars[urlEpPID] = ep12.ID()
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 1 {
		t.Fatalf("Unexpected services in response: %v", list)
	}
	if list[0].ID != ep12.ID() {
		t.Fatalf("Unexpected element in response: %v", list)
	}

	vars[urlEpPID] = "non-id"
	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 0 {
		t.Fatalf("Unexpected services in response: %v", list)
	}

	delete(vars, urlEpPID)
	err = ep11.Delete()
	if err != nil {
		t.Fatal(err)
	}
	err = ep12.Delete()
	if err != nil {
		t.Fatal(err)
	}
	err = ep21.Delete()
	if err != nil {
		t.Fatal(err)
	}

	li, errRsp = procGetServices(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	list = i2eL(li)
	if len(list) != 0 {
		t.Fatalf("Unexpected services in response: %v", list)
	}
}

func TestProcGetService(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, nw := createTestNetwork(t, "network")
	ep1, err := nw.CreateEndpoint("db")
	if err != nil {
		t.Fatal(err)
	}
	ep2, err := nw.CreateEndpoint("web")
	if err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{urlEpID: ""}
	_, errRsp := procGetService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure, but suceeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d, but got: %d", http.StatusBadRequest, errRsp.StatusCode)
	}

	vars[urlEpID] = "unknown-service-id"
	_, errRsp = procGetService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure, but suceeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d. (%v)", http.StatusNotFound, errRsp.StatusCode, errRsp)
	}

	vars[urlEpID] = ep1.ID()
	si, errRsp := procGetService(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	sv := i2e(si)
	if sv.ID != ep1.ID() {
		t.Fatalf("Unexpected service resource returned: %v", sv)
	}

	vars[urlEpID] = ep2.ID()
	si, errRsp = procGetService(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	sv = i2e(si)
	if sv.ID != ep2.ID() {
		t.Fatalf("Unexpected service resource returned: %v", sv)
	}
}

func TestProcPublishUnpublishService(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, _ := createTestNetwork(t, "network")
	vars := make(map[string]string)

	vbad, err := json.Marshal("bad service create data")
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp := procPublishService(c, vars, vbad)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	b, err := json.Marshal(servicePublish{Name: ""})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procPublishService(c, vars, b)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	b, err = json.Marshal(servicePublish{Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procPublishService(c, vars, b)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	b, err = json.Marshal(servicePublish{Name: "db", Network: "unknown-network"})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procPublishService(c, vars, b)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d. Got: %v", http.StatusNotFound, errRsp)
	}

	b, err = json.Marshal(servicePublish{Name: "", Network: "network"})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procPublishService(c, vars, b)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	b, err = json.Marshal(servicePublish{Name: "db", Network: "network"})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procPublishService(c, vars, b)
	if errRsp != &createdResponse {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}

	sp := servicePublish{
		Name:    "web",
		Network: "network",
		ExposedPorts: []types.TransportPort{
			types.TransportPort{Proto: types.TCP, Port: uint16(6000)},
			types.TransportPort{Proto: types.UDP, Port: uint16(500)},
			types.TransportPort{Proto: types.TCP, Port: uint16(700)},
		},
		PortMapping: []types.PortBinding{
			types.PortBinding{Proto: types.TCP, Port: uint16(1230), HostPort: uint16(37000)},
			types.PortBinding{Proto: types.UDP, Port: uint16(1200), HostPort: uint16(36000)},
			types.PortBinding{Proto: types.TCP, Port: uint16(1120), HostPort: uint16(35000)},
		},
	}
	b, err = json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	si, errRsp := procPublishService(c, vars, b)
	if errRsp != &createdResponse {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	sid := i2s(si)

	vars[urlEpID] = ""
	_, errRsp = procUnpublishService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	vars[urlEpID] = "unknown-service-id"
	_, errRsp = procUnpublishService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure but succeeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d. Got: %v", http.StatusNotFound, errRsp)
	}

	vars[urlEpID] = sid
	_, errRsp = procUnpublishService(c, vars, nil)
	if !errRsp.isOK() {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}

	_, errRsp = procGetService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure, but suceeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d. (%v)", http.StatusNotFound, errRsp.StatusCode, errRsp)
	}
}

func TestAttachDetachBackend(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, nw := createTestNetwork(t, "network")
	ep1, err := nw.CreateEndpoint("db")
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]string)

	vbad, err := json.Marshal("bad data")
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp := procAttachBackend(c, vars, vbad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "endpoint"
	bad, err := json.Marshal(endpointJoin{})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procAttachBackend(c, vars, bad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d. Got: %v", http.StatusNotFound, errRsp)
	}

	vars[urlEpID] = "db"
	_, errRsp = procGetSandbox(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure. Got %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d. Got: %v", http.StatusNotFound, errRsp)
	}

	vars[urlEpName] = "db"
	_, errRsp = procAttachBackend(c, vars, bad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	cid := "abcdefghi"
	sbox, err := c.NewSandbox(cid)
	sid := sbox.ID()
	defer sbox.Delete()

	jl := endpointJoin{SandboxID: sid}
	jlb, err := json.Marshal(jl)
	if err != nil {
		t.Fatal(err)
	}

	_, errRsp = procAttachBackend(c, vars, jlb)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure, got: %v", errRsp)
	}

	sli, errRsp := procGetSandboxes(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure, got: %v", errRsp)
	}
	sl := i2sbL(sli)
	if len(sl) != 1 {
		t.Fatalf("Did not find expected number of sandboxes attached to the service: %d", len(sl))
	}
	if sl[0].ContainerID != cid {
		t.Fatalf("Did not find expected sandbox attached to the service: %v", sl[0])
	}

	_, errRsp = procUnpublishService(c, vars, nil)
	if errRsp.isOK() {
		t.Fatalf("Expected failure but succeeded")
	}
	if errRsp.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected %d. Got: %v", http.StatusForbidden, errRsp)
	}

	vars[urlEpName] = "endpoint"
	_, errRsp = procDetachBackend(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d. Got: %v", http.StatusNotFound, errRsp)
	}

	vars[urlEpName] = "db"
	_, errRsp = procDetachBackend(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}

	vars[urlSbID] = sid
	_, errRsp = procDetachBackend(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure, got: %v", errRsp)
	}

	delete(vars, urlEpID)
	si, errRsp := procGetSandbox(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure, got: %v", errRsp)
	}
	sb := i2sb(si)
	if sb.ContainerID != cid {
		t.Fatalf("Did not find expected sandbox. Got %v", sb)
	}

	err = ep1.Delete()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDetectGetNetworksInvalidQueryComposition(t *testing.T) {
	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{urlNwName: "x", urlNwPID: "y"}
	_, errRsp := procGetNetworks(c, vars, nil)
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}
}

func TestDetectGetEndpointsInvalidQueryComposition(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, _ := createTestNetwork(t, "network")

	vars := map[string]string{urlNwName: "network", urlEpName: "x", urlEpPID: "y"}
	_, errRsp := procGetEndpoints(c, vars, nil)
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}
}

func TestDetectGetServicesInvalidQueryComposition(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, _ := createTestNetwork(t, "network")

	vars := map[string]string{urlNwName: "network", urlEpName: "x", urlEpPID: "y"}
	_, errRsp := procGetServices(c, vars, nil)
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d. Got: %v", http.StatusBadRequest, errRsp)
	}
}

func TestFindNetworkUtilPanic(t *testing.T) {
	defer checkPanic(t)
	findNetwork(nil, "", -1)
}

func TestFindNetworkUtil(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, nw := createTestNetwork(t, "network")
	nid := nw.ID()

	_, errRsp := findNetwork(c, "", byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d, but got: %d", http.StatusBadRequest, errRsp.StatusCode)
	}

	n, errRsp := findNetwork(c, nid, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	if n == nil {
		t.Fatalf("Unexpected nil libnetwork.Network")
	}
	if nid != n.ID() {
		t.Fatalf("Incorrect libnetwork.Network resource. It has different id: %v", n)
	}
	if "network" != n.Name() {
		t.Fatalf("Incorrect libnetwork.Network resource. It has different name: %v", n)
	}

	n, errRsp = findNetwork(c, "network", byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexpected failure: %v", errRsp)
	}
	if n == nil {
		t.Fatalf("Unexpected nil libnetwork.Network")
	}
	if nid != n.ID() {
		t.Fatalf("Incorrect libnetwork.Network resource. It has different id: %v", n)
	}
	if "network" != n.Name() {
		t.Fatalf("Incorrect libnetwork.Network resource. It has different name: %v", n)
	}

	if err := n.Delete(); err != nil {
		t.Fatalf("Failed to delete the network: %s", err.Error())
	}

	_, errRsp = findNetwork(c, nid, byID)
	if errRsp == &successResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findNetwork(c, "network", byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected to fail but succeeded")
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}
}

func TestCreateDeleteEndpoints(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	nc := networkCreate{Name: "firstNet", NetworkType: bridgeNetType}
	body, err := json.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]string)
	i, errRsp := procCreateNetwork(c, vars, body)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	nid := i2s(i)

	vbad, err := json.Marshal("bad endppoint create data")
	if err != nil {
		t.Fatal(err)
	}

	vars[urlNwName] = "firstNet"
	_, errRsp = procCreateEndpoint(c, vars, vbad)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}

	b, err := json.Marshal(endpointCreate{Name: ""})
	if err != nil {
		t.Fatal(err)
	}

	vars[urlNwName] = "secondNet"
	_, errRsp = procCreateEndpoint(c, vars, b)
	if errRsp == &createdResponse {
		t.Fatalf("Expected to fail but succeeded")
	}

	vars[urlNwName] = "firstNet"
	_, errRsp = procCreateEndpoint(c, vars, b)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure but succeeded: %v", errRsp)
	}

	b, err = json.Marshal(endpointCreate{Name: "firstEp"})
	if err != nil {
		t.Fatal(err)
	}

	i, errRsp = procCreateEndpoint(c, vars, b)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
	eid := i2s(i)

	_, errRsp = findEndpoint(c, "myNet", "firstEp", byName, byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure but succeeded: %v", errRsp)
	}

	ep0, errRsp := findEndpoint(c, nid, "firstEp", byID, byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep1, errRsp := findEndpoint(c, "firstNet", "firstEp", byName, byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep2, errRsp := findEndpoint(c, nid, eid, byID, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep3, errRsp := findEndpoint(c, "firstNet", eid, byName, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	if ep0.ID() != ep1.ID() || ep0.ID() != ep2.ID() || ep0.ID() != ep3.ID() {
		t.Fatalf("Diffenrent queries returned different endpoints: \nep0: %v\nep1: %v\nep2: %v\nep3: %v", ep0, ep1, ep2, ep3)
	}

	vars = make(map[string]string)
	vars[urlNwName] = ""
	vars[urlEpName] = "ep1"
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlNwName] = "firstNet"
	vars[urlEpName] = ""
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "ep2"
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "firstEp"
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	_, errRsp = findEndpoint(c, "firstNet", "firstEp", byName, byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
}

func TestJoinLeave(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	nb, err := json.Marshal(networkCreate{Name: "network", NetworkType: bridgeNetType})
	if err != nil {
		t.Fatal(err)
	}
	vars := make(map[string]string)
	_, errRsp := procCreateNetwork(c, vars, nb)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	eb, err := json.Marshal(endpointCreate{Name: "endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	vars[urlNwName] = "network"
	_, errRsp = procCreateEndpoint(c, vars, eb)
	if errRsp != &createdResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	vbad, err := json.Marshal("bad data")
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procJoinEndpoint(c, vars, vbad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "endpoint"
	bad, err := json.Marshal(endpointJoin{})
	if err != nil {
		t.Fatal(err)
	}
	_, errRsp = procJoinEndpoint(c, vars, bad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	cid := "abcdefghi"
	sb, err := c.NewSandbox(cid)
	defer sb.Delete()

	jl := endpointJoin{SandboxID: sb.ID()}
	jlb, err := json.Marshal(jl)
	if err != nil {
		t.Fatal(err)
	}

	vars = make(map[string]string)
	vars[urlNwName] = ""
	vars[urlEpName] = ""
	_, errRsp = procJoinEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlNwName] = "network"
	vars[urlEpName] = ""
	_, errRsp = procJoinEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "epoint"
	_, errRsp = procJoinEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlEpName] = "endpoint"
	key, errRsp := procJoinEndpoint(c, vars, jlb)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure, got: %v", errRsp)
	}

	keyStr := i2s(key)
	if keyStr == "" {
		t.Fatalf("Empty sandbox key")
	}
	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlNwName] = "network2"
	_, errRsp = procLeaveEndpoint(c, vars, vbad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	_, errRsp = procLeaveEndpoint(c, vars, bad)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	vars = make(map[string]string)
	vars[urlNwName] = ""
	vars[urlEpName] = ""
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	vars[urlNwName] = "network"
	vars[urlEpName] = ""
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	vars[urlEpName] = "2epoint"
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}
	vars[urlEpName] = "epoint"
	vars[urlCnID] = "who"
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	delete(vars, urlCnID)
	vars[urlEpName] = "endpoint"
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	vars[urlSbID] = sb.ID()
	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	_, errRsp = procLeaveEndpoint(c, vars, jlb)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, got: %v", errRsp)
	}

	_, errRsp = procDeleteEndpoint(c, vars, nil)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}
}

func TestFindEndpointUtilPanic(t *testing.T) {
	defer osl.SetupTestOSContext(t)()
	defer checkPanic(t)
	c, nw := createTestNetwork(t, "network")
	nid := nw.ID()
	findEndpoint(c, nid, "", byID, -1)
}

func TestFindServiceUtilPanic(t *testing.T) {
	defer osl.SetupTestOSContext(t)()
	defer checkPanic(t)
	c, _ := createTestNetwork(t, "network")
	findService(c, "random_service", -1)
}

func TestFindEndpointUtil(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, nw := createTestNetwork(t, "network")
	nid := nw.ID()

	ep, err := nw.CreateEndpoint("secondEp", nil)
	if err != nil {
		t.Fatal(err)
	}
	eid := ep.ID()

	_, errRsp := findEndpoint(c, nid, "", byID, byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Expected %d, but got: %d", http.StatusBadRequest, errRsp.StatusCode)
	}

	ep0, errRsp := findEndpoint(c, nid, "secondEp", byID, byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep1, errRsp := findEndpoint(c, "network", "secondEp", byName, byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep2, errRsp := findEndpoint(c, nid, eid, byID, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep3, errRsp := findEndpoint(c, "network", eid, byName, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep4, errRsp := findService(c, "secondEp", byName)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	ep5, errRsp := findService(c, eid, byID)
	if errRsp != &successResponse {
		t.Fatalf("Unexepected failure: %v", errRsp)
	}

	if ep0 != ep1 || ep0 != ep2 || ep0 != ep3 || ep0 != ep4 || ep0 != ep5 {
		t.Fatalf("Diffenrent queries returned different endpoints")
	}

	ep.Delete()

	_, errRsp = findEndpoint(c, nid, "secondEp", byID, byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findEndpoint(c, "network", "secondEp", byName, byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findEndpoint(c, nid, eid, byID, byID)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findEndpoint(c, "network", eid, byName, byID)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findService(c, "secondEp", byName)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}

	_, errRsp = findService(c, eid, byID)
	if errRsp == &successResponse {
		t.Fatalf("Expected failure, but got: %v", errRsp)
	}
	if errRsp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected %d, but got: %d", http.StatusNotFound, errRsp.StatusCode)
	}
}

func TestEndpointToService(t *testing.T) {
	r := &responseStatus{Status: "this is one endpoint", StatusCode: http.StatusOK}
	r = endpointToService(r)
	if r.Status != "this is one service" {
		t.Fatalf("endpointToService returned unexpected status string: %s", r.Status)
	}

	r = &responseStatus{Status: "this is one network", StatusCode: http.StatusOK}
	r = endpointToService(r)
	if r.Status != "this is one network" {
		t.Fatalf("endpointToService returned unexpected status string: %s", r.Status)
	}
}

func checkPanic(t *testing.T) {
	if r := recover(); r != nil {
		if _, ok := r.(runtime.Error); ok {
			panic(r)
		}
	} else {
		t.Fatalf("Expected to panic, but suceeded")
	}
}

func TestDetectNetworkTargetPanic(t *testing.T) {
	defer checkPanic(t)
	vars := make(map[string]string)
	detectNetworkTarget(vars)
}

func TestDetectEndpointTargetPanic(t *testing.T) {
	defer checkPanic(t)
	vars := make(map[string]string)
	detectEndpointTarget(vars)
}

func TestResponseStatus(t *testing.T) {
	list := []int{
		http.StatusBadGateway,
		http.StatusBadRequest,
		http.StatusConflict,
		http.StatusContinue,
		http.StatusExpectationFailed,
		http.StatusForbidden,
		http.StatusFound,
		http.StatusGatewayTimeout,
		http.StatusGone,
		http.StatusHTTPVersionNotSupported,
		http.StatusInternalServerError,
		http.StatusLengthRequired,
		http.StatusMethodNotAllowed,
		http.StatusMovedPermanently,
		http.StatusMultipleChoices,
		http.StatusNoContent,
		http.StatusNonAuthoritativeInfo,
		http.StatusNotAcceptable,
		http.StatusNotFound,
		http.StatusNotModified,
		http.StatusPartialContent,
		http.StatusPaymentRequired,
		http.StatusPreconditionFailed,
		http.StatusProxyAuthRequired,
		http.StatusRequestEntityTooLarge,
		http.StatusRequestTimeout,
		http.StatusRequestURITooLong,
		http.StatusRequestedRangeNotSatisfiable,
		http.StatusResetContent,
		http.StatusServiceUnavailable,
		http.StatusSwitchingProtocols,
		http.StatusTemporaryRedirect,
		http.StatusUnauthorized,
		http.StatusUnsupportedMediaType,
		http.StatusUseProxy,
	}
	for _, c := range list {
		r := responseStatus{StatusCode: c}
		if r.isOK() {
			t.Fatalf("isOK() returned true for code% d", c)
		}
	}

	r := responseStatus{StatusCode: http.StatusOK}
	if !r.isOK() {
		t.Fatalf("isOK() failed")
	}

	r = responseStatus{StatusCode: http.StatusCreated}
	if !r.isOK() {
		t.Fatalf("isOK() failed")
	}
}

// Local structs for end to end testing of api.go
type localReader struct {
	data  []byte
	beBad bool
}

func newLocalReader(data []byte) *localReader {
	lr := &localReader{data: make([]byte, len(data))}
	copy(lr.data, data)
	return lr
}

func (l *localReader) Read(p []byte) (n int, err error) {
	if l.beBad {
		return 0, errors.New("I am a bad reader")
	}
	if p == nil {
		return -1, fmt.Errorf("nil buffer passed")
	}
	if l.data == nil || len(l.data) == 0 {
		return 0, io.EOF
	}
	copy(p[:], l.data[:])
	return len(l.data), io.EOF
}

type localResponseWriter struct {
	body       []byte
	statusCode int
}

func newWriter() *localResponseWriter {
	return &localResponseWriter{}
}

func (f *localResponseWriter) Header() http.Header {
	return make(map[string][]string, 0)
}

func (f *localResponseWriter) Write(data []byte) (int, error) {
	if data == nil {
		return -1, fmt.Errorf("nil data passed")
	}

	f.body = make([]byte, len(data))
	copy(f.body, data)

	return len(f.body), nil
}

func (f *localResponseWriter) WriteHeader(c int) {
	f.statusCode = c
}

func TestwriteJSON(t *testing.T) {
	testCode := 55
	testData, err := json.Marshal("test data")
	if err != nil {
		t.Fatal(err)
	}

	rsp := newWriter()
	writeJSON(rsp, testCode, testData)
	if rsp.statusCode != testCode {
		t.Fatalf("writeJSON() failed to set the status code. Expected %d. Got %d", testCode, rsp.statusCode)
	}
	if !bytes.Equal(testData, rsp.body) {
		t.Fatalf("writeJSON() failed to set the body. Expected %s. Got %s", testData, rsp.body)
	}

}

func TestHttpHandlerUninit(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}

	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	h := &httpHandler{c: c}
	h.initRouter()
	if h.r == nil {
		t.Fatalf("initRouter() did not initialize the router")
	}

	rsp := newWriter()
	req, err := http.NewRequest("GET", "/v1.19/networks", nil)
	if err != nil {
		t.Fatal(err)
	}

	handleRequest := NewHTTPHandler(nil)
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusServiceUnavailable {
		t.Fatalf("Expected (%d). Got (%d): %s", http.StatusServiceUnavailable, rsp.statusCode, rsp.body)
	}

	handleRequest = NewHTTPHandler(c)

	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected (%d). Got: (%d): %s", http.StatusOK, rsp.statusCode, rsp.body)
	}

	var list []*networkResource
	err = json.Unmarshal(rsp.body, &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("Expected empty list. Got %v", list)
	}

	n, err := c.NewNetwork(bridgeNetType, "didietro", nil)
	if err != nil {
		t.Fatal(err)
	}
	nwr := buildNetworkResource(n)
	expected, err := json.Marshal([]*networkResource{nwr})
	if err != nil {
		t.Fatal(err)
	}

	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}
	if len(rsp.body) == 0 {
		t.Fatalf("Empty list of networks")
	}
	if bytes.Equal(rsp.body, expected) {
		t.Fatalf("Incorrect list of networks in response's body")
	}
}

func TestHttpHandlerBadBody(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	rsp := newWriter()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	handleRequest := NewHTTPHandler(c)

	req, err := http.NewRequest("POST", "/v1.19/networks", &localReader{beBad: true})
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusBadRequest {
		t.Fatalf("Unexpected status code. Expected (%d). Got (%d): %s.", http.StatusBadRequest, rsp.statusCode, string(rsp.body))
	}

	body := []byte{}
	lr := newLocalReader(body)
	req, err = http.NewRequest("POST", "/v1.19/networks", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusBadRequest {
		t.Fatalf("Unexpected status code. Expected (%d). Got (%d): %s.", http.StatusBadRequest, rsp.statusCode, string(rsp.body))
	}
}

func TestEndToEnd(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	rsp := newWriter()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	err = c.ConfigureNetworkDriver(bridgeNetType, nil)
	if err != nil {
		t.Fatal(err)
	}

	handleRequest := NewHTTPHandler(c)

	ops := options.Generic{
		netlabel.EnableIPv6: true,
		netlabel.GenericData: map[string]string{
			"BridgeName":            "cdef",
			"FixedCIDRv6":           "fe80:2000::1/64",
			"EnableIPv6":            "true",
			"Mtu":                   "1460",
			"EnableIPTables":        "true",
			"AddressIP":             "172.28.30.254/16",
			"EnableUserlandProxy":   "true",
			"AllowNonDefaultBridge": "true",
		},
	}

	// Create network
	nc := networkCreate{Name: "network-fiftyfive", NetworkType: bridgeNetType, Options: ops}
	body, err := json.Marshal(nc)
	if err != nil {
		t.Fatal(err)
	}
	lr := newLocalReader(body)
	req, err := http.NewRequest("POST", "/v1.19/networks", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusCreated {
		t.Fatalf("Unexpectded status code. Expected (%d). Got (%d): %s.", http.StatusCreated, rsp.statusCode, string(rsp.body))
	}
	if len(rsp.body) == 0 {
		t.Fatalf("Empty response body")
	}

	var nid string
	err = json.Unmarshal(rsp.body, &nid)
	if err != nil {
		t.Fatal(err)
	}

	// Query networks collection
	req, err = http.NewRequest("GET", "/v1.19/networks?name=", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}
	var list []*networkResource
	err = json.Unmarshal(rsp.body, &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("Expected empty list. Got %v", list)
	}

	req, err = http.NewRequest("GET", "/v1.19/networks", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}

	b0 := make([]byte, len(rsp.body))
	copy(b0, rsp.body)

	req, err = http.NewRequest("GET", "/v1.19/networks?name=network-fiftyfive", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}

	if !bytes.Equal(b0, rsp.body) {
		t.Fatalf("Expected same body from GET /networks and GET /networks?name=<nw> when only network <nw> exist.")
	}

	// Query network by name
	req, err = http.NewRequest("GET", "/v1.19/networks?name=culo", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("Expected empty list. Got %v", list)
	}

	req, err = http.NewRequest("GET", "/v1.19/networks?name=network-fiftyfive", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatalf("Expected non empty list")
	}
	if list[0].Name != "network-fiftyfive" || nid != list[0].ID {
		t.Fatalf("Incongruent resource found: %v", list[0])
	}

	// Query network by partial id
	chars := []byte(nid)
	partial := string(chars[0 : len(chars)/2])
	req, err = http.NewRequest("GET", "/v1.19/networks?partial-id="+partial, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &list)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatalf("Expected non empty list")
	}
	if list[0].Name != "network-fiftyfive" || nid != list[0].ID {
		t.Fatalf("Incongruent resource found: %v", list[0])
	}

	// Get network by id
	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	var nwr networkResource
	err = json.Unmarshal(rsp.body, &nwr)
	if err != nil {
		t.Fatal(err)
	}
	if nwr.Name != "network-fiftyfive" || nid != nwr.ID {
		t.Fatalf("Incongruent resource found: %v", nwr)
	}

	// Create endpoint
	eb, err := json.Marshal(endpointCreate{Name: "ep-TwentyTwo"})
	if err != nil {
		t.Fatal(err)
	}

	lr = newLocalReader(eb)
	req, err = http.NewRequest("POST", "/v1.19/networks/"+nid+"/endpoints", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusCreated {
		t.Fatalf("Unexpectded status code. Expected (%d). Got (%d): %s.", http.StatusCreated, rsp.statusCode, string(rsp.body))
	}
	if len(rsp.body) == 0 {
		t.Fatalf("Empty response body")
	}

	var eid string
	err = json.Unmarshal(rsp.body, &eid)
	if err != nil {
		t.Fatal(err)
	}

	// Query endpoint(s)
	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid+"/endpoints", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}

	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid+"/endpoints?name=bla", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}
	var epList []*endpointResource
	err = json.Unmarshal(rsp.body, &epList)
	if err != nil {
		t.Fatal(err)
	}
	if len(epList) != 0 {
		t.Fatalf("Expected empty list. Got %v", epList)
	}

	// Query endpoint by name
	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid+"/endpoints?name=ep-TwentyTwo", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &epList)
	if err != nil {
		t.Fatal(err)
	}
	if len(epList) == 0 {
		t.Fatalf("Empty response body")
	}
	if epList[0].Name != "ep-TwentyTwo" || eid != epList[0].ID {
		t.Fatalf("Incongruent resource found: %v", epList[0])
	}

	// Query endpoint by partial id
	chars = []byte(eid)
	partial = string(chars[0 : len(chars)/2])
	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid+"/endpoints?partial-id="+partial, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &epList)
	if err != nil {
		t.Fatal(err)
	}
	if len(epList) == 0 {
		t.Fatalf("Empty response body")
	}
	if epList[0].Name != "ep-TwentyTwo" || eid != epList[0].ID {
		t.Fatalf("Incongruent resource found: %v", epList[0])
	}

	// Get endpoint by id
	req, err = http.NewRequest("GET", "/v1.19/networks/"+nid+"/endpoints/"+eid, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	var epr endpointResource
	err = json.Unmarshal(rsp.body, &epr)
	if err != nil {
		t.Fatal(err)
	}
	if epr.Name != "ep-TwentyTwo" || epr.ID != eid {
		t.Fatalf("Incongruent resource found: %v", epr)
	}

	// Store two container ids and one partial ids
	cid1 := "container10010000000"
	cid2 := "container20010000000"
	chars = []byte(cid1)
	cpid1 := string(chars[0 : len(chars)/2])

	// Create sandboxes
	sb1, err := json.Marshal(sandboxCreate{ContainerID: cid1})
	if err != nil {
		t.Fatal(err)
	}

	lr = newLocalReader(sb1)
	req, err = http.NewRequest("POST", "/v5.22/sandboxes", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusCreated {
		t.Fatalf("Unexpectded status code. Expected (%d). Got (%d): %s.", http.StatusCreated, rsp.statusCode, string(rsp.body))
	}
	if len(rsp.body) == 0 {
		t.Fatalf("Empty response body")
	}
	// Get sandbox id and partial id
	var sid1 string
	err = json.Unmarshal(rsp.body, &sid1)
	if err != nil {
		t.Fatal(err)
	}

	sb2, err := json.Marshal(sandboxCreate{ContainerID: cid2})
	if err != nil {
		t.Fatal(err)
	}

	lr = newLocalReader(sb2)
	req, err = http.NewRequest("POST", "/v5.22/sandboxes", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusCreated {
		t.Fatalf("Unexpectded status code. Expected (%d). Got (%d): %s.", http.StatusCreated, rsp.statusCode, string(rsp.body))
	}
	if len(rsp.body) == 0 {
		t.Fatalf("Empty response body")
	}
	// Get sandbox id and partial id
	var sid2 string
	err = json.Unmarshal(rsp.body, &sid2)
	if err != nil {
		t.Fatal(err)
	}
	chars = []byte(sid2)
	spid2 := string(chars[0 : len(chars)/2])

	// Query sandboxes
	req, err = http.NewRequest("GET", "/sandboxes", nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Expected StatusOK. Got (%d): %s", rsp.statusCode, rsp.body)
	}

	var sbList []*sandboxResource
	err = json.Unmarshal(rsp.body, &sbList)
	if err != nil {
		t.Fatal(err)
	}
	if len(sbList) != 2 {
		t.Fatalf("Expected 2 elements in list. Got %v", sbList)
	}

	// Get sandbox by id
	req, err = http.NewRequest("GET", "/sandboxes/"+sid1, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	var sbr sandboxResource
	err = json.Unmarshal(rsp.body, &sbr)
	if err != nil {
		t.Fatal(err)
	}
	if sbr.ContainerID != cid1 {
		t.Fatalf("Incongruent resource found: %v", sbr)
	}

	// Query sandbox by partial sandbox id
	req, err = http.NewRequest("GET", "/sandboxes?partial-id="+spid2, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &sbList)
	if err != nil {
		t.Fatal(err)
	}
	if len(sbList) == 0 {
		t.Fatalf("Empty response body")
	}
	if sbList[0].ID != sid2 {
		t.Fatalf("Incongruent resource found: %v", sbList[0])
	}

	// Query sandbox by container id
	req, err = http.NewRequest("GET", "/sandboxes?container-id="+cid2, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &sbList)
	if err != nil {
		t.Fatal(err)
	}
	if len(sbList) == 0 {
		t.Fatalf("Empty response body")
	}
	if sbList[0].ContainerID != cid2 {
		t.Fatalf("Incongruent resource found: %v", sbList[0])
	}

	// Query sandbox by partial container id
	req, err = http.NewRequest("GET", "/sandboxes?partial-container-id="+cpid1, nil)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)
	if rsp.statusCode != http.StatusOK {
		t.Fatalf("Unexpectded failure: (%d): %s", rsp.statusCode, rsp.body)
	}

	err = json.Unmarshal(rsp.body, &sbList)
	if err != nil {
		t.Fatal(err)
	}
	if len(sbList) == 0 {
		t.Fatalf("Empty response body")
	}
	if sbList[0].ContainerID != cid1 {
		t.Fatalf("Incongruent resource found: %v", sbList[0])
	}
}

func TestEndToEndErrorMessage(t *testing.T) {
	defer osl.SetupTestOSContext(t)()

	rsp := newWriter()

	c, err := libnetwork.New()
	if err != nil {
		t.Fatal(err)
	}
	handleRequest := NewHTTPHandler(c)

	body := []byte{}
	lr := newLocalReader(body)
	req, err := http.NewRequest("POST", "/v1.19/networks", lr)
	if err != nil {
		t.Fatal(err)
	}
	handleRequest(rsp, req)

	if len(rsp.body) == 0 {
		t.Fatalf("Empty response body.")
	}
	empty := []byte("\"\"")
	if bytes.Equal(empty, bytes.TrimSpace(rsp.body)) {
		t.Fatalf("Empty response error message.")
	}
}

type bre struct{}

func (b *bre) Error() string {
	return "I am a bad request error"
}
func (b *bre) BadRequest() {}

type nfe struct{}

func (n *nfe) Error() string {
	return "I am a not found error"
}
func (n *nfe) NotFound() {}

type forb struct{}

func (f *forb) Error() string {
	return "I am a bad request error"
}
func (f *forb) Forbidden() {}

type notimpl struct{}

func (nip *notimpl) Error() string {
	return "I am a not implemented error"
}
func (nip *notimpl) NotImplemented() {}

type inter struct{}

func (it *inter) Error() string {
	return "I am a internal error"
}
func (it *inter) Internal() {}

type tout struct{}

func (to *tout) Error() string {
	return "I am a timeout error"
}
func (to *tout) Timeout() {}

type noserv struct{}

func (nos *noserv) Error() string {
	return "I am a no service error"
}
func (nos *noserv) NoService() {}

type notclassified struct{}

func (noc *notclassified) Error() string {
	return "I am a non classified error"
}

func TestErrorConversion(t *testing.T) {
	if convertNetworkError(new(bre)).StatusCode != http.StatusBadRequest {
		t.Fatalf("Failed to recognize BadRequest error")
	}

	if convertNetworkError(new(nfe)).StatusCode != http.StatusNotFound {
		t.Fatalf("Failed to recognize NotFound error")
	}

	if convertNetworkError(new(forb)).StatusCode != http.StatusForbidden {
		t.Fatalf("Failed to recognize Forbidden error")
	}

	if convertNetworkError(new(notimpl)).StatusCode != http.StatusNotImplemented {
		t.Fatalf("Failed to recognize NotImplemented error")
	}

	if convertNetworkError(new(inter)).StatusCode != http.StatusInternalServerError {
		t.Fatalf("Failed to recognize Internal error")
	}

	if convertNetworkError(new(tout)).StatusCode != http.StatusRequestTimeout {
		t.Fatalf("Failed to recognize Timeout error")
	}

	if convertNetworkError(new(noserv)).StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("Failed to recognize No Service error")
	}

	if convertNetworkError(new(notclassified)).StatusCode != http.StatusInternalServerError {
		t.Fatalf("Failed to recognize not classified error as Internal error")
	}
}

func TestFieldRegex(t *testing.T) {
	pr := regexp.MustCompile(regex)
	qr := regexp.MustCompile(`^` + qregx + `$`) // mux compiles it like this

	if pr.MatchString("") {
		t.Fatalf("Unexpected match")
	}
	if !qr.MatchString("") {
		t.Fatalf("Unexpected match failure")
	}

	if pr.MatchString(":") {
		t.Fatalf("Unexpected match")
	}
	if qr.MatchString(":") {
		t.Fatalf("Unexpected match")
	}

	if pr.MatchString(".") {
		t.Fatalf("Unexpected match")
	}
	if qr.MatchString(".") {
		t.Fatalf("Unexpected match")
	}
}
