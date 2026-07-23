package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// jbd2 replay: committed-but-not-checkpointed metadata lives only in the
// journal. A mount would replay it; so do we, into fs.overlay, before any
// metadata is trusted. Everything in the journal is BIG-endian.
//
// Scope: replay of committed transactions with revoke handling, tag formats
// with and without csum_v3/64bit. Block checksums are parsed but not
// verified — a torn transaction self-terminates via its missing commit
// block, and old-wrap blocks are rejected by sequence number, which is the
// same integrity model debugfs applies. Orphan-inode processing is skipped:
// krilld's data disks never have deleted-but-open files at tail time.
const (
	jbd2Magic = 0xc03b3998

	jbd2Descriptor = 1
	jbd2Commit     = 2
	jbd2SuperV1    = 3
	jbd2SuperV2    = 4
	jbd2Revoke     = 5

	jbd2FlagEscape   = 1
	jbd2FlagSameUUID = 2
	jbd2FlagLastTag  = 8

	jbd2Feat64Bit  = 0x0002
	jbd2FeatCsumV3 = 0x0010
)

func (fs *FS) replayJournal() error {
	if fs.journalNo == 0 {
		return nil
	}
	ino, err := fs.inodeBytes(fs.journalNo)
	if err != nil {
		return err
	}
	journal, err := fs.readInodeFile(ino)
	if err != nil {
		return err
	}
	if len(journal) < int(fs.blockSize) {
		return errors.New("journal file too small")
	}

	jsb := journal[:32+16]
	if binary.BigEndian.Uint32(jsb[0:]) != jbd2Magic {
		return errors.New("bad journal magic")
	}
	if bt := binary.BigEndian.Uint32(jsb[4:]); bt != jbd2SuperV1 && bt != jbd2SuperV2 {
		return fmt.Errorf("journal block 0 has type %d, want superblock", bt)
	}
	jBlockSize := int64(binary.BigEndian.Uint32(jsb[12:]))
	maxLen := int64(binary.BigEndian.Uint32(jsb[16:]))
	first := int64(binary.BigEndian.Uint32(jsb[20:]))
	seq := binary.BigEndian.Uint32(jsb[24:])
	start := int64(binary.BigEndian.Uint32(jsb[28:]))
	incompat := binary.BigEndian.Uint32(jsb[40:])
	if jBlockSize != fs.blockSize {
		return fmt.Errorf("journal block size %d != fs block size %d", jBlockSize, fs.blockSize)
	}
	if start == 0 {
		return nil // clean journal: nothing to replay
	}
	if first < 1 || start < first || maxLen > int64(len(journal))/jBlockSize {
		return errors.New("corrupt journal geometry")
	}

	jblock := func(n int64) []byte {
		return journal[n*jBlockSize : (n+1)*jBlockSize]
	}
	next := func(n int64) int64 {
		n++
		if n >= maxLen {
			n = first
		}
		return n
	}
	tagSize := int64(8)
	if incompat&jbd2FeatCsumV3 != 0 {
		tagSize = 16
	} else if incompat&jbd2Feat64Bit != 0 {
		tagSize = 12
	}

	// A transaction: descriptor(s) + data blocks, terminated by a commit
	// block carrying the same sequence number. We walk the log twice —
	// pass 1 finds complete transactions and collects revokes; pass 2
	// applies tags in order, honoring the revoke table.
	type tagRef struct {
		fsBlock  int64
		logBlock int64 // where the data lives in the journal
		escaped  bool
	}
	type txn struct {
		seq  uint32
		tags []tagRef
	}
	var txns []txn
	revoked := map[int64]uint32{} // fs block -> highest seq that revoked it

	cur, expect := start, seq
	var open *txn
scan:
	for {
		blk := jblock(cur)
		if binary.BigEndian.Uint32(blk[0:]) != jbd2Magic {
			break // end of log
		}
		btype := binary.BigEndian.Uint32(blk[4:])
		bseq := binary.BigEndian.Uint32(blk[8:])
		if bseq != expect {
			break // old wrap or future garbage
		}
		switch btype {
		case jbd2Descriptor:
			if open == nil {
				open = &txn{seq: bseq}
			}
			off := int64(12)
			for off+tagSize <= jBlockSize {
				t := blk[off:]
				fsBlock := int64(binary.BigEndian.Uint32(t[0:]))
				var flags uint32
				if tagSize == 16 { // csum_v3 layout: blocknr, flags, blocknr_high, checksum
					flags = binary.BigEndian.Uint32(t[4:])
					fsBlock |= int64(binary.BigEndian.Uint32(t[8:])) << 32
				} else { // classic: blocknr, checksum16, flags16 [, blocknr_high]
					flags = uint32(binary.BigEndian.Uint16(t[6:]))
					if tagSize == 12 {
						fsBlock |= int64(binary.BigEndian.Uint32(t[8:])) << 32
					}
				}
				off += tagSize
				if flags&jbd2FlagSameUUID == 0 {
					off += 16
				}
				cur = next(cur)
				if cur == start {
					break scan // wrapped fully around: corrupt
				}
				open.tags = append(open.tags, tagRef{
					fsBlock:  fsBlock,
					logBlock: cur,
					escaped:  flags&jbd2FlagEscape != 0,
				})
				if flags&jbd2FlagLastTag != 0 {
					break
				}
			}
		case jbd2Revoke:
			count := int64(binary.BigEndian.Uint32(blk[12:]))
			if count > jBlockSize {
				break scan
			}
			rsize := int64(4)
			if incompat&jbd2Feat64Bit != 0 {
				rsize = 8
			}
			for off := int64(16); off+rsize <= count; off += rsize {
				var b int64
				if rsize == 8 {
					b = int64(binary.BigEndian.Uint64(blk[off:]))
				} else {
					b = int64(binary.BigEndian.Uint32(blk[off:]))
				}
				if revoked[b] < bseq {
					revoked[b] = bseq
				}
			}
		case jbd2Commit:
			if open != nil && open.seq == bseq {
				txns = append(txns, *open)
			}
			open = nil
			expect++
		default:
			break scan
		}
		cur = next(cur)
		if cur == start {
			break // full circle
		}
	}

	for _, t := range txns {
		for _, tag := range t.tags {
			if rs, ok := revoked[tag.fsBlock]; ok && rs >= t.seq {
				continue
			}
			data := append([]byte(nil), jblock(tag.logBlock)...)
			if tag.escaped {
				binary.BigEndian.PutUint32(data[0:], jbd2Magic)
			}
			fs.overlay[tag.fsBlock] = data
		}
	}
	return nil
}
