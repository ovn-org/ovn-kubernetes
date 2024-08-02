package observability_lib

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"

	"github.com/ovn-org/ovn-kubernetes/go-controller/observability-lib/sampledecoder"
)

const (
	PSAMPLE_GENL_NAME            = "psample"
	PSAMPLE_NL_MCGRP_SAMPLE_NAME = "packets"
)

const (
	PSAMPLE_ATTR_IIFINDEX = iota
	PSAMPLE_ATTR_OIFINDEX
	PSAMPLE_ATTR_ORIGSIZE
	PSAMPLE_ATTR_SAMPLE_GROUP
	PSAMPLE_ATTR_GROUP_SEQ
	PSAMPLE_ATTR_SAMPLE_RATE
	PSAMPLE_ATTR_DATA
	PSAMPLE_ATTR_GROUP_REFCOUNT
	PSAMPLE_ATTR_TUNNEL
	PSAMPLE_ATTR_PAD
	PSAMPLE_ATTR_OUT_TC     /* u16 */
	PSAMPLE_ATTR_OUT_TC_OCC /* u64, bytes */
	PSAMPLE_ATTR_LATENCY    /* u64, nanoseconds */
	PSAMPLE_ATTR_TIMESTAMP  /* u64, nanoseconds */
	PSAMPLE_ATTR_PROTO      /* u16 */
	PSAMPLE_ATTR_USER_COOKIE
	__PSAMPLE_ATTR_MAX
)

type SampleReader struct {
	enableDecoder   bool
	logCookie       bool
	printFullPacket bool
	addOVSCollector bool
	srcIP, dstIP    string
	outputFile      string

	decoder   *sampledecoder.SampleDecoder
	cookieStr []string
}

func NewSampleReader(enableDecoder, logCookie, printFullPacket, addOVSCollector bool, srcIP, dstIP, outputFile string) *SampleReader {
	r := &SampleReader{
		enableDecoder:   enableDecoder,
		logCookie:       logCookie,
		printFullPacket: printFullPacket,
		addOVSCollector: addOVSCollector,
		srcIP:           srcIP,
		dstIP:           dstIP,
		outputFile:      outputFile,
	}
	if logCookie {
		r.cookieStr = make([]string, 2)
	}
	return r
}

func (r *SampleReader) ReadSamples(ctx context.Context) error {
	if r.enableDecoder {
		var err error
		// currently only local nbdb connection is supported.
		nbdbSocketPath := "/var/run/ovn/ovnnb_db.sock"
		if r.addOVSCollector {
			r.decoder, err = sampledecoder.NewSampleDecoderWithDefaultCollector(ctx, nbdbSocketPath, "ovnk-debug", 123)
			if err != nil {
				return fmt.Errorf("error creating decoder: %w", err)
			}
			defer r.decoder.Shutdown()
		} else {
			r.decoder, err = sampledecoder.NewSampleDecoder(ctx, nbdbSocketPath)
			if err != nil {
				return fmt.Errorf("error creating decoder: %w", err)
			}
		}
	}
	var writer io.Writer
	if r.outputFile != "" {
		file, err := os.Create(r.outputFile)
		if err != nil {
			return fmt.Errorf("error creating output file: %w", err)
		}
		defer file.Close()
		writer = bufio.NewWriter(file)
	} else {
		writer = os.Stdout
	}
	l := log.New(writer, "", log.Ldate|log.Ltime|log.Lmicroseconds)
	printlnFunc := func(a ...any) {
		l.Println(a...)
	}

	fam, err := netlink.GenlFamilyGet(PSAMPLE_GENL_NAME)
	if err != nil {
		return fmt.Errorf("error getting netlink family %s: %w", PSAMPLE_GENL_NAME, err)
	}
	if len(fam.Groups) == 0 {
		return fmt.Errorf("no mcast groups found for %s", PSAMPLE_GENL_NAME)
	}
	var ovsGroupID uint32
	for _, group := range fam.Groups {
		if group.Name == PSAMPLE_NL_MCGRP_SAMPLE_NAME {
			ovsGroupID = group.ID
		}
	}
	if ovsGroupID == 0 {
		return fmt.Errorf("no mcast group found for %s", PSAMPLE_NL_MCGRP_SAMPLE_NAME)
	} else {
		fmt.Printf("Found group %s, id %d\n", PSAMPLE_NL_MCGRP_SAMPLE_NAME, ovsGroupID)
	}
	sock, err := nl.Subscribe(nl.GENL_ID_CTRL, uint(ovsGroupID))
	if err != nil {
		return fmt.Errorf("error subscribing to netlink group %d: %w", ovsGroupID, err)
	}

	// Otherwise sock.Receive() will be blocking and won't return on context close
	if err = unix.SetNonblock(sock.GetFd(), true); err != nil {
		return fmt.Errorf("error setting non-blocking mode: %w", err)
	}

	defer func() {
		sock.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			msgs, _, err := sock.Receive()
			if err != nil {
				if err == syscall.EAGAIN {
					continue
				}
				printlnFunc("ERROR: receive failed:", err)
				continue
			}
			if err = r.parseMsg(msgs, printlnFunc); err != nil {
				printlnFunc("ERROR: ", err)
			}
		}
	}
}

func getHostEndian() binary.ByteOrder {
	buf := [2]byte{}
	*(*uint16)(unsafe.Pointer(&buf[0])) = uint16(0xABCD)

	switch buf {
	case [2]byte{0xCD, 0xAB}:
		return binary.LittleEndian
	case [2]byte{0xAB, 0xCD}:
		return binary.BigEndian
	default:
		panic("Could not determine native endianness.")
	}
}

var hostEndian = getHostEndian()

func (r *SampleReader) parseMsg(msgs []syscall.NetlinkMessage, printlnFunc func(a ...any)) error {
	for _, msg := range msgs {
		var packetStr, sampleStr string
		data := msg.Data[nl.SizeofGenlmsg:]
		for attr := range nl.ParseAttributes(data) {
			if r.logCookie && attr.Type == PSAMPLE_ATTR_SAMPLE_GROUP {
				if uint64(len(attr.Value)) == 4 {
					g := uint32(0)
					// group is encoded using host endian
					err := binary.Read(bytes.NewReader(attr.Value), hostEndian, &g)
					if err != nil {
						return err
					}
					r.cookieStr[0] = fmt.Sprintf("group_id=%v", g)
				}
			}
			if attr.Type == PSAMPLE_ATTR_USER_COOKIE && (r.logCookie || r.decoder != nil) {
				if uint64(len(attr.Value)) == sampledecoder.CookieSize {
					c := sampledecoder.Cookie{}
					err := binary.Read(bytes.NewReader(attr.Value), sampledecoder.SampleEndian, &c)
					if err != nil {
						return err
					}
					if r.logCookie {
						r.cookieStr[1] = fmt.Sprintf("obs_domain=%v, obs_point=%v",
							c.ObsDomainID, c.ObsPointID)
					}
					if r.decoder != nil {
						decoded, err := r.decoder.DecodeCookieIDs(c.ObsDomainID, c.ObsPointID)
						if err != nil {
							sampleStr = fmt.Sprintf("decoding failed: %v", err)
						} else {
							sampleStr = fmt.Sprintf("OVN-K message: %s", decoded)
						}
					}
				}
			}
			if attr.Type == PSAMPLE_ATTR_DATA {
				packet := gopacket.NewPacket(attr.Value, layers.LayerTypeEthernet, gopacket.Lazy)
				networkLayer := packet.NetworkLayer().NetworkFlow()
				if r.printFullPacket {
					packetStr = packet.String()
				} else {
					packetStr = fmt.Sprintf("src=%s, dst=%v\n",
						networkLayer.Src().String(), networkLayer.Dst().String())
				}
				if r.srcIP != "" && r.srcIP != networkLayer.Src().String() {
					return nil
				}
				if r.dstIP != "" && r.dstIP != networkLayer.Dst().String() {
					return nil
				}
			}
		}
		if r.logCookie {
			printlnFunc(strings.Join(r.cookieStr, ", "))
		}
		if r.decoder != nil {
			printlnFunc(sampleStr)
		}
		printlnFunc(packetStr)
	}
	return nil
}
