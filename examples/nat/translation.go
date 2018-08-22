// Copyright 2017-2018 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nat

import (
	"time"

	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"
)

// Tuple is a pair of address and port.
type Tuple struct {
	addr uint32
	port uint16
}

func (pp *portPair) allocateNewEgressConnection(protocol uint8, privEntry *Tuple) (Tuple, error) {
	pp.mutex.Lock()

	port, err := pp.allocNewPort(protocol)
	if err != nil {
		pp.mutex.Unlock()
		return Tuple{}, err
	}

	publicAddr := pp.PublicPort.Subnet.Addr
	pubEntry := Tuple{
		addr: publicAddr,
		port: uint16(port),
	}

	pp.PublicPort.portmap[protocol][port] = portMapEntry{
		lastused:             time.Now(),
		addr:                 publicAddr,
		finCount:             0,
		terminationDirection: 0,
		static:               false,
	}

	// Add lookup entries for packet translation
	pp.PublicPort.translationTable[protocol].Store(pubEntry, *privEntry)
	pp.PrivatePort.translationTable[protocol].Store(*privEntry, pubEntry)

	pp.mutex.Unlock()
	return pubEntry, nil
}

// PublicToPrivateTranslation does ingress translation.
func PublicToPrivateTranslation(pkt *packet.Packet, ctx flow.UserContext) uint {
	pi := ctx.(pairIndex)
	pp := &Natconfig.PortPairs[pi.index]
	port := &pp.PublicPort

	port.dumpPacket(pkt, dirSEND)

	// Parse packet type and address
	dir, pktVLAN, pktIPv4 := port.parsePacketAndCheckARP(pkt)
	if pktIPv4 == nil {
		return dir
	}

	// Create a lookup key from packet destination address and port
	pktTCP, pktUDP, pktICMP := pkt.ParseAllKnownL4ForIPv4()
	protocol := pktIPv4.NextProtoID
	pub2priKey, dir := port.generateLookupKeyFromDstAndHandleICMP(pkt, pktIPv4, pktTCP, pktUDP, pktICMP)
	if pub2priKey == nil {
		return dir
	}

	// Do lookup
	v, found := port.translationTable[protocol].Load(*pub2priKey)
	// For ingress connections packets are allowed only if a
	// connection has been previosly established with a egress
	// (private to public) packet. So if lookup fails, this incoming
	// packet is ignored.
	if !found {
		port.dumpPacket(pkt, dirDROP)
		return dirDROP
	}
	value := v.(Tuple)

	// Check whether connection is too old
	if port.portmap[protocol][pub2priKey.port].static || time.Since(port.portmap[protocol][pub2priKey.port].lastused) <= connectionTimeout {
		port.portmap[protocol][pub2priKey.port].lastused = time.Now()
	} else {
		// There was no transfer on this port for too long
		// time. We don't allow it any more
		pp.mutex.Lock()
		pp.deleteOldConnection(protocol, int(pub2priKey.port))
		pp.mutex.Unlock()
		port.dumpPacket(pkt, dirDROP)
		return dirDROP
	}

	if value.addr != 0 {
		// Check whether TCP connection could be reused
		if protocol == common.TCPNumber {
			pp.checkTCPTermination(pktTCP, int(pub2priKey.port), pub2pri)
		}

		// Do packet translation
		mac, found := port.opposite.getMACForIP(value.addr)
		if !found {
			port.dumpPacket(pkt, dirDROP)
			return dirDROP
		}
		pkt.Ether.DAddr = mac
		pkt.Ether.SAddr = port.SrcMACAddress
		if pktVLAN != nil {
			pktVLAN.SetVLANTagIdentifier(port.opposite.Vlan)
		}
		pktIPv4.DstAddr = packet.SwapBytesUint32(value.addr)
		setPacketDstPort(pkt, value.port, pktTCP, pktUDP, pktICMP)

		port.dumpPacket(pkt, dirSEND)
		return dirSEND
	} else {
		port.dumpPacket(pkt, dirKNI)
		return dirKNI
	}
}

// PrivateToPublicTranslation does egress translation.
func PrivateToPublicTranslation(pkt *packet.Packet, ctx flow.UserContext) uint {
	pi := ctx.(pairIndex)
	pp := &Natconfig.PortPairs[pi.index]
	port := &pp.PrivatePort

	port.dumpPacket(pkt, dirSEND)

	// Parse packet type and address
	dir, pktVLAN, pktIPv4 := port.parsePacketAndCheckARP(pkt)
	if pktIPv4 == nil {
		return dir
	}

	// Create a lookup key from packet source address and port
	pktTCP, pktUDP, pktICMP := pkt.ParseAllKnownL4ForIPv4()
	protocol := pktIPv4.NextProtoID
	pri2pubKey, dir := port.generateLookupKeyFromSrcAndHandleICMP(pkt, pktIPv4, pktTCP, pktUDP, pktICMP)
	if pri2pubKey == nil {
		return dir
	}

	// If traffic is directed at private interface IP and KNI is
	// present, this traffic is directed to KNI
	if port.KNIName != "" && port.Subnet.Addr == packet.SwapBytesUint32(pktIPv4.DstAddr) {
		port.dumpPacket(pkt, dirKNI)
		return dirKNI
	}

	// Do lookup
	var value Tuple
	v, found := port.translationTable[protocol].Load(*pri2pubKey)
	if !found {
		var err error
		// Store new local network entry in ARP cache
		port.arpTable.Store(pri2pubKey.addr, pkt.Ether.SAddr)
		// Allocate new connection from private to public network
		value, err = pp.allocateNewEgressConnection(protocol, pri2pubKey)

		if err != nil {
			println("Warning! Failed to allocate new connection", err)
			port.dumpPacket(pkt, dirDROP)
			return dirDROP
		}
	} else {
		value = v.(Tuple)
		pp.PublicPort.portmap[protocol][value.port].lastused = time.Now()
	}

	if value.addr != 0 {
		// Check whether TCP connection could be reused
		if pktTCP != nil {
			pp.checkTCPTermination(pktTCP, int(value.port), pri2pub)
		}

		// Do packet translation
		mac, found := port.opposite.getMACForIP(packet.SwapBytesUint32(pktIPv4.DstAddr))
		if !found {
			port.dumpPacket(pkt, dirDROP)
			return dirDROP
		}
		pkt.Ether.DAddr = mac
		pkt.Ether.SAddr = port.SrcMACAddress
		if pktVLAN != nil {
			pktVLAN.SetVLANTagIdentifier(port.opposite.Vlan)
		}
		pktIPv4.SrcAddr = packet.SwapBytesUint32(value.addr)
		setPacketSrcPort(pkt, value.port, pktTCP, pktUDP, pktICMP)

		port.dumpPacket(pkt, dirSEND)
		return dirSEND
	} else {
		port.dumpPacket(pkt, dirKNI)
		return dirKNI
	}
}

// Used to generate key in public to private translation
func (port *ipv4Port) generateLookupKeyFromDstAndHandleICMP(pkt *packet.Packet, pktIPv4 *packet.IPv4Hdr, pktTCP *packet.TCPHdr, pktUDP *packet.UDPHdr, pktICMP *packet.ICMPHdr) (*Tuple, uint) {
	key := Tuple{
		addr: packet.SwapBytesUint32(pktIPv4.DstAddr),
	}
	// Parse packet destination port
	if pktTCP != nil {
		key.port = packet.SwapBytesUint16(pktTCP.DstPort)
	} else if pktUDP != nil {
		key.port = packet.SwapBytesUint16(pktUDP.DstPort)
	} else if pktICMP != nil {
		// Check if this ICMP packet destination is NAT itself. If
		// yes, reply back with ICMP and stop packet processing.
		key.port = packet.SwapBytesUint16(pktICMP.Identifier)
		dir := port.handleICMP(pkt, &key)
		if dir != dirSEND {
			return nil, dir
		}
	} else {
		port.dumpPacket(pkt, dirDROP)
		return nil, dirDROP
	}
	return &key, dirSEND
}

// Used to generate key in private to public translation
func (port *ipv4Port) generateLookupKeyFromSrcAndHandleICMP(pkt *packet.Packet, pktIPv4 *packet.IPv4Hdr, pktTCP *packet.TCPHdr, pktUDP *packet.UDPHdr, pktICMP *packet.ICMPHdr) (*Tuple, uint) {
	key := Tuple{
		addr: packet.SwapBytesUint32(pktIPv4.SrcAddr),
	}

	// Parse packet source port
	if pktTCP != nil {
		key.port = packet.SwapBytesUint16(pktTCP.SrcPort)
	} else if pktUDP != nil {
		key.port = packet.SwapBytesUint16(pktUDP.SrcPort)
	} else if pktICMP != nil {
		// Check if this ICMP packet destination is NAT itself. If
		// yes, reply back with ICMP and stop packet processing or
		// direct to KNI if KNI is present.
		dir := port.handleICMP(pkt, nil)
		if dir != dirSEND {
			return nil, dir
		}
		key.port = packet.SwapBytesUint16(pktICMP.Identifier)
	} else {
		port.dumpPacket(pkt, dirDROP)
		return nil, dirDROP
	}
	return &key, dirSEND
}

func setPacketDstPort(pkt *packet.Packet, port uint16, pktTCP *packet.TCPHdr, pktUDP *packet.UDPHdr, pktICMP *packet.ICMPHdr) {
	if pktTCP != nil {
		pktTCP.DstPort = packet.SwapBytesUint16(port)
		setIPv4TCPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	} else if pktUDP != nil {
		pktUDP.DstPort = packet.SwapBytesUint16(port)
		setIPv4UDPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	} else {
		pktICMP.Identifier = packet.SwapBytesUint16(port)
		setIPv4ICMPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	}
}

func setPacketSrcPort(pkt *packet.Packet, port uint16, pktTCP *packet.TCPHdr, pktUDP *packet.UDPHdr, pktICMP *packet.ICMPHdr) {
	if pktTCP != nil {
		pktTCP.SrcPort = packet.SwapBytesUint16(port)
		setIPv4TCPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	} else if pktUDP != nil {
		pktUDP.SrcPort = packet.SwapBytesUint16(port)
		setIPv4UDPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	} else {
		pktICMP.Identifier = packet.SwapBytesUint16(port)
		setIPv4ICMPChecksum(pkt, !NoCalculateChecksum, !NoHWTXChecksum)
	}
}

// Simple check for FIN or RST in TCP
func (pp *portPair) checkTCPTermination(hdr *packet.TCPHdr, port int, dir terminationDirection) {
	if hdr.TCPFlags&common.TCPFlagFin != 0 {
		// First check for FIN
		pp.mutex.Lock()

		pme := &pp.PublicPort.portmap[common.TCPNumber][port]
		if pme.finCount == 0 {
			pme.finCount = 1
			pme.terminationDirection = dir
		} else if pme.finCount == 1 && pme.terminationDirection == ^dir {
			pme.finCount = 2
		}

		pp.mutex.Unlock()
	} else if hdr.TCPFlags&common.TCPFlagRst != 0 {
		// RST means that connection is terminated immediately
		pp.mutex.Lock()
		pp.deleteOldConnection(common.TCPNumber, port)
		pp.mutex.Unlock()
	} else if hdr.TCPFlags&common.TCPFlagAck != 0 {
		// Check for ACK last so that if there is also FIN,
		// termination doesn't happen. Last ACK should come without
		// FIN
		pp.mutex.Lock()

		pme := &pp.PublicPort.portmap[common.TCPNumber][port]
		if pme.finCount == 2 {
			pp.deleteOldConnection(common.TCPNumber, port)
			// Set some time while port cannot be used before
			// connection timeout is reached
			pme.lastused = time.Now().Add(time.Duration(portReuseTimeout - connectionTimeout))
		}

		pp.mutex.Unlock()
	}
}

func (port *ipv4Port) parsePacketAndCheckARP(pkt *packet.Packet) (dir uint, vhdr *packet.VLANHdr, iphdr *packet.IPv4Hdr) {
	pktVLAN := pkt.ParseL3CheckVLAN()
	pktIPv4 := pkt.GetIPv4CheckVLAN()
	if pktIPv4 == nil {
		arp := pkt.GetARPCheckVLAN()
		if arp != nil {
			dir := port.handleARP(pkt)
			port.dumpPacket(pkt, dir)
			return dir, pktVLAN, nil
		}
		// We don't currently support anything except for IPv4 and ARP
		port.dumpPacket(pkt, dirDROP)
		return dirDROP, pktVLAN, nil
	}
	return dirSEND, pktVLAN, pktIPv4
}

func (port *ipv4Port) handleARP(pkt *packet.Packet) uint {
	arp := pkt.GetARPNoCheck()

	if packet.SwapBytesUint16(arp.Operation) != packet.ARPRequest {
		if packet.SwapBytesUint16(arp.Operation) == packet.ARPReply {
			ipv4 := packet.SwapBytesUint32(packet.ArrayToIPv4(arp.SPA))
			port.arpTable.Store(ipv4, arp.SHA)
		}
		if port.KNIName != "" {
			return dirKNI
		}
		return dirDROP
	}

	// If there is a KNI interface, direct all ARP traffic to it
	if port.KNIName != "" {
		return dirKNI
	}

	// Check that someone is asking about MAC of my IP address and HW
	// address is blank in request
	if packet.BytesToIPv4(arp.TPA[0], arp.TPA[1], arp.TPA[2], arp.TPA[3]) != packet.SwapBytesUint32(port.Subnet.Addr) {
		println("Warning! Got an ARP packet with target IPv4 address", StringIPv4Array(arp.TPA),
			"different from IPv4 address on interface. Should be", StringIPv4Int(port.Subnet.Addr),
			". ARP request ignored.")
		return dirDROP
	}
	if arp.THA != [common.EtherAddrLen]byte{} {
		println("Warning! Got an ARP packet with non-zero MAC address", StringMAC(arp.THA),
			". ARP request ignored.")
		return dirDROP
	}

	// Prepare an answer to this request
	answerPacket, err := packet.NewPacket()
	if err != nil {
		common.LogFatal(common.Debug, err)
	}

	packet.InitARPReplyPacket(answerPacket, port.SrcMACAddress, arp.SHA, packet.ArrayToIPv4(arp.TPA), packet.ArrayToIPv4(arp.SPA))
	vlan := pkt.GetVLAN()
	if vlan != nil {
		answerPacket.AddVLANTag(packet.SwapBytesUint16(vlan.TCI))
	}

	port.dumpPacket(answerPacket, dirSEND)
	answerPacket.SendPacket(port.Index)

	return dirDROP
}

func (port *ipv4Port) getMACForIP(ip uint32) (macAddress, bool) {
	v, found := port.arpTable.Load(ip)
	if found {
		return macAddress(v.([common.EtherAddrLen]byte)), true
	}
	port.sendARPRequest(ip)
	return macAddress{}, false
}

func (port *ipv4Port) sendARPRequest(ip uint32) {
	// Prepare an answer to this request
	requestPacket, err := packet.NewPacket()
	if err != nil {
		common.LogFatal(common.Debug, err)
	}

	packet.InitARPRequestPacket(requestPacket, port.SrcMACAddress,
		packet.SwapBytesUint32(port.Subnet.Addr), packet.SwapBytesUint32(ip))
	if port.Vlan != 0 {
		requestPacket.AddVLANTag(port.Vlan)
	}

	port.dumpPacket(requestPacket, dirSEND)
	requestPacket.SendPacket(port.Index)
}

func (port *ipv4Port) handleICMP(pkt *packet.Packet, key *Tuple) uint {
	ipv4 := pkt.GetIPv4NoCheck()

	// Check that received ICMP packet is addressed at this host. If
	// not, packet should be translated
	if packet.SwapBytesUint32(ipv4.DstAddr) != port.Subnet.Addr {
		return dirSEND
	}

	icmp := pkt.GetICMPNoCheck()

	// If there is KNI interface, direct all ICMP traffic which
	// doesn't have an active translation entry
	if port.KNIName != "" {
		if key != nil {
			_, ok := port.translationTable[common.ICMPNumber].Load(*key)
			if !ok || time.Since(port.portmap[common.ICMPNumber][key.port].lastused) > connectionTimeout {
				return dirKNI
			}
		}
	}

	// Check that received ICMP packet is echo request packet. We
	// don't support any other messages yet, so process them in normal
	// NAT way. Maybe these are packets which should be passed through
	// translation.
	if icmp.Type != common.ICMPTypeEchoRequest || icmp.Code != 0 {
		return dirSEND
	}

	// Return a packet back to sender
	answerPacket, err := packet.NewPacket()
	if err != nil {
		common.LogFatal(common.Debug, err)
	}
	packet.GeneratePacketFromByte(answerPacket, pkt.GetRawPacketBytes())

	answerPacket.ParseL3CheckVLAN()
	swapAddrIPv4(answerPacket)
	answerPacket.ParseL4ForIPv4()
	(answerPacket.GetICMPNoCheck()).Type = common.ICMPTypeEchoResponse
	setIPv4ICMPChecksum(answerPacket, !NoCalculateChecksum, !NoHWTXChecksum)

	port.dumpPacket(answerPacket, dirSEND)
	answerPacket.SendPacket(port.Index)
	return dirDROP
}
