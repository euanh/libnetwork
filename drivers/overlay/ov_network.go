package overlay

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/osl"
	"github.com/docker/libnetwork/types"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

type networkTable map[string]*network

type subnet struct {
	once      *sync.Once
	vxlanName string
	brName    string
	vni       uint32
	initErr   error
	subnetIP  *net.IPNet
	gwIP      *net.IPNet
}

type network struct {
	id        string
	dbIndex   uint64
	dbExists  bool
	sbox      osl.Sandbox
	endpoints endpointTable
	driver    *driver
	joinCnt   int
	once      *sync.Once
	initEpoch int
	initErr   error
	subnets   []*subnet
	sync.Mutex
}

func (d *driver) CreateNetwork(id string, option map[string]interface{}, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	var err error
	if id == "" {
		return fmt.Errorf("invalid network id")
	}

	if err = d.configure(); err != nil {
		return err
	}

	n := &network{
		id:        id,
		driver:    d,
		endpoints: endpointTable{},
		once:      &sync.Once{},
		subnets:   []*subnet{},
	}

	for _, ipd := range ipV4Data {
		s := &subnet{
			subnetIP: ipd.Pool,
			gwIP:     ipd.Gateway,
			once:     &sync.Once{},
		}
		n.subnets = append(n.subnets, s)
	}

	for {
		// If the datastore has the network object already
		// there is no need to do a write.
		err = d.store.GetObject(datastore.Key(n.Key()...), n)
		if err == nil || err != datastore.ErrKeyNotFound {
			break
		}

		err = n.writeToStore()
		if err == nil || err != datastore.ErrKeyModified {
			break
		}
	}

	if err != nil {
		return fmt.Errorf("failed to update data store for network %v: %v", n.id, err)
	}
	d.addNetwork(n)

	return nil
}

func (d *driver) createNetworkfromStore(nid string) (*network, error) {
	n := &network{
		id:        nid,
		driver:    d,
		endpoints: endpointTable{},
		once:      &sync.Once{},
		subnets:   []*subnet{},
	}

	err := d.store.GetObject(datastore.Key(n.Key()...), n)
	if err != nil {
		return nil, fmt.Errorf("unable to get network %q from data store, %v", nid, err)
	}
	return n, nil
}

func (d *driver) DeleteNetwork(nid string) error {
	if nid == "" {
		return fmt.Errorf("invalid network id")
	}

	n := d.network(nid)
	if n == nil {
		return fmt.Errorf("could not find network with id %s", nid)
	}

	d.deleteNetwork(nid)

	return n.releaseVxlanID()
}

func (n *network) joinSandbox() error {
	n.Lock()
	if n.joinCnt != 0 {
		n.joinCnt++
		n.Unlock()
		return nil
	}
	n.Unlock()

	// If there is a race between two go routines here only one will win
	// the other will wait.
	n.once.Do(func() {
		// save the error status of initSandbox in n.initErr so that
		// all the racing go routines are able to know the status.
		n.initErr = n.initSandbox()
	})

	return n.initErr
}

func (n *network) joinSubnetSandbox(s *subnet) error {

	s.once.Do(func() {
		s.initErr = n.initSubnetSandbox(s)
	})
	// Increment joinCnt in all the goroutines only when the one time initSandbox
	// was a success.
	n.Lock()
	if s.initErr == nil {
		n.joinCnt++
	}
	err := s.initErr
	n.Unlock()

	return err
}

func (n *network) leaveSandbox() {
	n.Lock()
	n.joinCnt--
	if n.joinCnt != 0 {
		n.Unlock()
		return
	}

	// We are about to destroy sandbox since the container is leaving the network
	// Reinitialize the once variable so that we will be able to trigger one time
	// sandbox initialization(again) when another container joins subsequently.
	n.once = &sync.Once{}
	for _, s := range n.subnets {
		s.once = &sync.Once{}
	}
	n.Unlock()

	n.destroySandbox()
}

func (n *network) destroySandbox() {
	sbox := n.sandbox()
	if sbox != nil {
		for _, iface := range sbox.Info().Interfaces() {
			iface.Remove()
		}

		for _, s := range n.subnets {
			if s.vxlanName != "" {
				err := deleteVxlan(s.vxlanName)
				if err != nil {
					logrus.Warnf("could not cleanup sandbox properly: %v", err)
				}
			}
		}
		sbox.Destroy()
	}
}

func (n *network) initSubnetSandbox(s *subnet) error {
	// create a bridge and vxlan device for this subnet and move it to the sandbox
	brName, err := netutils.GenerateIfaceName("bridge", 7)
	if err != nil {
		return err
	}
	sbox := n.sandbox()

	if err := sbox.AddInterface(brName, "br",
		sbox.InterfaceOptions().Address(s.gwIP),
		sbox.InterfaceOptions().Bridge(true)); err != nil {
		return fmt.Errorf("bridge creation in sandbox failed for subnet %q: %v", s.subnetIP.IP.String(), err)
	}

	vxlanName, err := createVxlan(n.vxlanID(s))
	if err != nil {
		return err
	}

	if err := sbox.AddInterface(vxlanName, "vxlan",
		sbox.InterfaceOptions().Master(brName)); err != nil {
		return fmt.Errorf("vxlan interface creation failed for subnet %q: %v", s.subnetIP.IP.String(), err)
	}

	n.Lock()
	s.vxlanName = vxlanName
	s.brName = brName
	n.Unlock()

	return nil
}

func (n *network) initSandbox() error {
	n.Lock()
	n.initEpoch++
	n.Unlock()

	sbox, err := osl.NewSandbox(
		osl.GenerateKey(fmt.Sprintf("%d-", n.initEpoch)+n.id), true)
	if err != nil {
		return fmt.Errorf("could not create network sandbox: %v", err)
	}

	n.setSandbox(sbox)

	n.driver.peerDbUpdateSandbox(n.id)

	var nlSock *nl.NetlinkSocket
	sbox.InvokeFunc(func() {
		nlSock, err = nl.Subscribe(syscall.NETLINK_ROUTE, syscall.RTNLGRP_NEIGH)
		if err != nil {
			err = fmt.Errorf("failed to subscribe to neighbor group netlink messages")
		}
	})

	go n.watchMiss(nlSock)
	return nil
}

func (n *network) watchMiss(nlSock *nl.NetlinkSocket) {
	for {
		msgs, err := nlSock.Receive()
		if err != nil {
			logrus.Errorf("Failed to receive from netlink: %v ", err)
			continue
		}

		for _, msg := range msgs {
			if msg.Header.Type != syscall.RTM_GETNEIGH && msg.Header.Type != syscall.RTM_NEWNEIGH {
				continue
			}

			neigh, err := netlink.NeighDeserialize(msg.Data)
			if err != nil {
				logrus.Errorf("Failed to deserialize netlink ndmsg: %v", err)
				continue
			}

			if neigh.IP.To16() != nil {
				continue
			}

			if neigh.State&(netlink.NUD_STALE|netlink.NUD_INCOMPLETE) == 0 {
				continue
			}

			mac, IPmask, vtep, err := n.driver.resolvePeer(n.id, neigh.IP)
			if err != nil {
				logrus.Errorf("could not resolve peer %q: %v", neigh.IP, err)
				continue
			}

			if err := n.driver.peerAdd(n.id, "dummy", neigh.IP, IPmask, mac, vtep, true); err != nil {
				logrus.Errorf("could not add neighbor entry for missed peer %q: %v", neigh.IP, err)
			}
		}
	}
}

func (d *driver) addNetwork(n *network) {
	d.Lock()
	d.networks[n.id] = n
	d.Unlock()
}

func (d *driver) deleteNetwork(nid string) {
	d.Lock()
	delete(d.networks, nid)
	d.Unlock()
}

func (d *driver) network(nid string) *network {
	d.Lock()
	defer d.Unlock()

	return d.networks[nid]
}

func (n *network) sandbox() osl.Sandbox {
	n.Lock()
	defer n.Unlock()

	return n.sbox
}

func (n *network) setSandbox(sbox osl.Sandbox) {
	n.Lock()
	n.sbox = sbox
	n.Unlock()
}

func (n *network) vxlanID(s *subnet) uint32 {
	n.Lock()
	defer n.Unlock()

	return s.vni
}

func (n *network) setVxlanID(s *subnet, vni uint32) {
	n.Lock()
	s.vni = vni
	n.Unlock()
}

func (n *network) Key() []string {
	return []string{"overlay", "network", n.id}
}

func (n *network) KeyPrefix() []string {
	return []string{"overlay", "network"}
}

func (n *network) Value() []byte {
	overlayNetmap := make(map[string]interface{})

	s := n.subnets[0]
	if s == nil {
		logrus.Errorf("Network %s has no subnets", n.id)
		return []byte{}
	}

	overlayNetmap["subnetIP"] = s.subnetIP.String()
	overlayNetmap["gwIP"] = s.gwIP.String()
	overlayNetmap["vni"] = s.vni

	b, err := json.Marshal(overlayNetmap)
	if err != nil {
		return []byte{}
	}

	return b
}

func (n *network) Index() uint64 {
	return n.dbIndex
}

func (n *network) SetIndex(index uint64) {
	n.dbIndex = index
	n.dbExists = true
}

func (n *network) Exists() bool {
	return n.dbExists
}

func (n *network) Skip() bool {
	return false
}

func (n *network) SetValue(value []byte) error {
	var (
		overlayNetmap map[string]interface{}
		err           error
	)

	err = json.Unmarshal(value, &overlayNetmap)
	if err != nil {
		return err
	}

	subnetIPstr := overlayNetmap["subnetIP"].(string)
	gwIPstr := overlayNetmap["gwIP"].(string)
	vni := uint32(overlayNetmap["vni"].(float64))

	subnetIP, _ := types.ParseCIDR(subnetIPstr)
	gwIP, _ := types.ParseCIDR(gwIPstr)

	// If the network is being created by reading from the
	// datastore subnets have to created. If the network
	// already exists update only the subnets' vni field
	if len(n.subnets) == 0 {
		s := &subnet{
			subnetIP: subnetIP,
			gwIP:     gwIP,
			vni:      vni,
			once:     &sync.Once{},
		}
		n.subnets = append(n.subnets, s)
		return nil
	}

	sNet := n.getMatchingSubnet(subnetIP)
	if sNet != nil {
		if vni != 0 {
			sNet.vni = vni
		}
	}
	return nil
}

func (n *network) DataScope() datastore.DataScope {
	return datastore.GlobalScope
}

func (n *network) writeToStore() error {
	return n.driver.store.PutObjectAtomic(n)
}

func (n *network) releaseVxlanID() error {
	if n.driver.store == nil {
		return fmt.Errorf("no datastore configured. cannot release vxlan id")
	}

	if len(n.subnets) == 0 {
		return nil
	}

	if err := n.driver.store.DeleteObjectAtomic(n); err != nil {
		if err == datastore.ErrKeyModified || err == datastore.ErrKeyNotFound {
			// In both the above cases we can safely assume that the key has been removed by some other
			// instance and so simply get out of here
			return nil
		}

		return fmt.Errorf("failed to delete network to vxlan id map: %v", err)
	}

	for _, s := range n.subnets {
		n.driver.vxlanIdm.Release(n.vxlanID(s))
		n.setVxlanID(s, 0)
	}
	return nil
}

func (n *network) obtainVxlanID(s *subnet) error {
	//return if the subnet already has a vxlan id assigned
	if s.vni != 0 {
		return nil
	}

	if n.driver.store == nil {
		return fmt.Errorf("no datastore configured. cannot obtain vxlan id")
	}

	for {
		if err := n.driver.store.GetObject(datastore.Key(n.Key()...), n); err != nil {
			return fmt.Errorf("getting network %q from datastore failed %v", n.id, err)
		}

		if s.vni == 0 {
			vxlanID, err := n.driver.vxlanIdm.GetID()
			if err != nil {
				return fmt.Errorf("failed to allocate vxlan id: %v", err)
			}

			n.setVxlanID(s, vxlanID)
			if err := n.writeToStore(); err != nil {
				n.driver.vxlanIdm.Release(n.vxlanID(s))
				n.setVxlanID(s, 0)
				if err == datastore.ErrKeyModified {
					continue
				}
				return fmt.Errorf("network %q failed to update data store: %v", n.id, err)
			}
			return nil
		}
		return nil
	}
}

// getSubnetforIP returns the subnet to which the given IP belongs
func (n *network) getSubnetforIP(ip *net.IPNet) *subnet {
	for _, s := range n.subnets {
		// first check if the mask lengths are the same
		i, _ := s.subnetIP.Mask.Size()
		j, _ := ip.Mask.Size()
		if i != j {
			continue
		}
		if s.subnetIP.Contains(ip.IP) {
			return s
		}
	}
	return nil
}

// getMatchingSubnet return the network's subnet that matches the input
func (n *network) getMatchingSubnet(ip *net.IPNet) *subnet {
	if ip == nil {
		return nil
	}
	for _, s := range n.subnets {
		// first check if the mask lengths are the same
		i, _ := s.subnetIP.Mask.Size()
		j, _ := ip.Mask.Size()
		if i != j {
			continue
		}
		if s.subnetIP.IP.Equal(ip.IP) {
			return s
		}
	}
	return nil
}
