package dnsproxy

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// HostResolver resolves L2TP endpoint names through explicitly configured DoH
// channels. It never falls back to the host resolver or dnsmasq.
type HostResolver struct{ channels []NodeChannel }

func NewHostResolver(channels ...NodeChannel) *HostResolver {
	filtered := make([]NodeChannel, 0, len(channels))
	for _, channel := range channels {
		if channel != nil {
			filtered = append(filtered, channel)
		}
	}
	return &HostResolver{channels: filtered}
}

func (resolver *HostResolver) ResolveIPv4(ctx context.Context, host string) (netip.Addr, error) {
	query, err := hostAQuery(host)
	if err != nil || resolver == nil || len(resolver.channels) == 0 {
		return netip.Addr{}, errors.New("bootstrap hostname is invalid")
	}
	for _, channel := range resolver.channels {
		response, err := channel.Resolve(ctx, query)
		if err == nil {
			if address, parseErr := parseHostAResponse(query, response); parseErr == nil {
				return address, nil
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return netip.Addr{}, errors.New("bootstrap hostname resolution failed")
}

func hostAQuery(host string) ([]byte, error) {
	if len(host) == 0 || len(host) > 253 || strings.HasSuffix(host, ".") {
		return nil, errors.New("invalid DNS hostname")
	}
	labels := strings.Split(host, ".")
	query := make([]byte, 12, 12+len(host)+6)
	if _, err := rand.Read(query[:2]); err != nil {
		return nil, errors.New("DNS query identity failed")
	}
	query[2] = 0x01
	query[5] = 0x01
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return nil, errors.New("invalid DNS hostname")
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' {
				return nil, errors.New("invalid DNS hostname")
			}
		}
		query = append(query, byte(len(label)))
		query = append(query, label...)
	}
	query = append(query, 0, 0, 1, 0, 1)
	return query, nil
}

func parseHostAResponse(query, response []byte) (netip.Addr, error) {
	if len(query) < 12 || len(response) < 12 || binary.BigEndian.Uint16(query[:2]) != binary.BigEndian.Uint16(response[:2]) ||
		response[2]&0x80 == 0 || response[3]&0x0f != 0 || binary.BigEndian.Uint16(response[4:6]) != 1 {
		return netip.Addr{}, errors.New("invalid DNS response")
	}
	answerCount := int(binary.BigEndian.Uint16(response[6:8]))
	if answerCount < 1 || answerCount > 64 {
		return netip.Addr{}, errors.New("invalid DNS response")
	}
	offset, err := skipDNSName(response, 12)
	if err != nil || offset+4 > len(response) {
		return netip.Addr{}, errors.New("invalid DNS response")
	}
	offset += 4
	for index := 0; index < answerCount; index++ {
		offset, err = skipDNSName(response, offset)
		if err != nil || offset+10 > len(response) {
			return netip.Addr{}, errors.New("invalid DNS response")
		}
		recordType := binary.BigEndian.Uint16(response[offset : offset+2])
		recordClass := binary.BigEndian.Uint16(response[offset+2 : offset+4])
		dataLength := int(binary.BigEndian.Uint16(response[offset+8 : offset+10]))
		offset += 10
		if dataLength < 0 || offset+dataLength > len(response) {
			return netip.Addr{}, errors.New("invalid DNS response")
		}
		if recordType == 1 && recordClass == 1 && dataLength == 4 {
			var bytes [4]byte
			copy(bytes[:], response[offset:offset+4])
			address := netip.AddrFrom4(bytes)
			if address.IsGlobalUnicast() && !address.IsLoopback() && !address.IsLinkLocalUnicast() {
				return address, nil
			}
		}
		offset += dataLength
	}
	return netip.Addr{}, errors.New("DNS response has no usable IPv4 address")
}

func skipDNSName(message []byte, offset int) (int, error) {
	for labels := 0; labels <= 127; labels++ {
		if offset >= len(message) {
			return 0, errors.New("invalid DNS name")
		}
		length := int(message[offset])
		switch {
		case length == 0:
			return offset + 1, nil
		case length&0xc0 == 0xc0:
			if offset+1 >= len(message) {
				return 0, errors.New("invalid DNS name")
			}
			return offset + 2, nil
		case length&0xc0 != 0 || length > 63 || offset+1+length > len(message):
			return 0, errors.New("invalid DNS name")
		default:
			offset += 1 + length
		}
	}
	return 0, errors.New("invalid DNS name")
}
