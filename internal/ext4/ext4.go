// Package ext4 is a minimal READ-ONLY ext4 parser: superblock, group
// descriptors, extent trees, directories — enough to pull a file out of a
// disk image by path. It exists so the host agent can tail a guest's SQLite
// WAL from OUTSIDE the VM (protocol doc §1.1): the guest writes to its data
// disk via virtio; krilld reads the backing image file directly and never
// asks the guest for anything.
//
// Reading an image the guest is actively writing is safe for exactly one
// reason: the payload this package is used to fetch (SQLite WAL frames)
// carries its own cumulative checksum chain, so torn or stale bytes are
// detected by the layer above (internal/sqlitewal), never trusted. Metadata
// staleness is handled by replaying the jbd2 journal (see journal.go):
// after a guest fsync, the freshest metadata may exist ONLY as committed
// journal transactions, exactly like the crashed-disk case a mount would
// recover. Open replays the journal into an in-memory overlay by default.
//
// Deliberately unsupported (krilld formats every data disk itself, so these
// never occur): inline_data, meta_bg, encryption, bigalloc. Non-extent
// (ext2-style block-map) files are rejected per-inode.
package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	ErrNotExist = errors.New("ext4: file does not exist")
	ErrNotExt4  = errors.New("ext4: bad superblock magic")
)

const (
	sbOffset = 1024
	rootIno  = 2

	// incompat feature flags
	featIncompat64Bit      = 0x0080
	featIncompatExtents    = 0x0040
	featIncompatMetaBG     = 0x0010
	featIncompatInlineData = 0x8000
	featIncompatEncrypt    = 0x10000
	featIncompatJournalDev = 0x0008

	inodeFlagExtents    = 0x80000
	inodeFlagInlineData = 0x10000000
)

type FS struct {
	r         io.ReaderAt
	blockSize int64
	inodesPer uint32
	inodeSize uint32
	firstData uint32
	descSize  uint32
	journalNo uint32
	// overlay holds journal-replayed metadata blocks: reads check it first.
	overlay map[int64][]byte
}

type Options struct {
	// SkipJournal opens without replaying the jbd2 journal. The fast-poll
	// tail path uses this (bounded metadata staleness, self-validating
	// payload); decisive reads — freeze catch-up, restore — must not.
	SkipJournal bool
}

func Open(r io.ReaderAt, opts Options) (*FS, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, sbOffset); err != nil {
		return nil, fmt.Errorf("ext4: reading superblock: %w", err)
	}
	if binary.LittleEndian.Uint16(sb[0x38:]) != 0xEF53 {
		return nil, ErrNotExt4
	}
	logBlock := binary.LittleEndian.Uint32(sb[0x18:])
	if logBlock > 6 { // 1 KiB .. 64 KiB
		return nil, fmt.Errorf("ext4: impossible block size 2^(10+%d)", logBlock)
	}
	fs := &FS{
		r:         r,
		blockSize: int64(1024) << logBlock,
		firstData: binary.LittleEndian.Uint32(sb[0x14:]),
		inodesPer: binary.LittleEndian.Uint32(sb[0x28:]),
		inodeSize: uint32(binary.LittleEndian.Uint16(sb[0x58:])),
		journalNo: binary.LittleEndian.Uint32(sb[0xE0:]),
		descSize:  32,
		overlay:   map[int64][]byte{},
	}
	incompat := binary.LittleEndian.Uint32(sb[0x60:])
	for flag, name := range map[uint32]string{
		featIncompatMetaBG:     "meta_bg",
		featIncompatInlineData: "inline_data",
		featIncompatEncrypt:    "encrypt",
		featIncompatJournalDev: "journal_dev",
	} {
		if incompat&flag != 0 {
			return nil, fmt.Errorf("ext4: unsupported feature %s", name)
		}
	}
	if incompat&featIncompat64Bit != 0 {
		if ds := binary.LittleEndian.Uint16(sb[0xFE:]); ds >= 32 {
			fs.descSize = uint32(ds)
		} else {
			fs.descSize = 64
		}
	}
	if fs.inodesPer == 0 || fs.inodeSize < 128 {
		return nil, errors.New("ext4: corrupt superblock geometry")
	}
	if !opts.SkipJournal {
		if err := fs.replayJournal(); err != nil {
			return nil, fmt.Errorf("ext4: journal replay: %w", err)
		}
	}
	return fs, nil
}

// readBlock returns one filesystem block, preferring the journal overlay.
func (fs *FS) readBlock(n int64) ([]byte, error) {
	if b, ok := fs.overlay[n]; ok {
		return b, nil
	}
	b := make([]byte, fs.blockSize)
	if _, err := fs.r.ReadAt(b, n*fs.blockSize); err != nil {
		return nil, fmt.Errorf("ext4: reading block %d: %w", n, err)
	}
	return b, nil
}

// inodeBytes fetches inode number n (1-based) from its group's inode table.
func (fs *FS) inodeBytes(n uint32) ([]byte, error) {
	if n == 0 {
		return nil, errors.New("ext4: inode 0")
	}
	group := (n - 1) / fs.inodesPer
	index := int64((n - 1) % fs.inodesPer)

	// Group descriptor table starts in the block after the superblock.
	gdtBlock := int64(fs.firstData) + 1
	descOff := int64(group) * int64(fs.descSize)
	descBlock, err := fs.readBlock(gdtBlock + descOff/fs.blockSize)
	if err != nil {
		return nil, err
	}
	d := descBlock[descOff%fs.blockSize:]
	tableBlock := int64(binary.LittleEndian.Uint32(d[8:]))
	if fs.descSize >= 64 {
		tableBlock |= int64(binary.LittleEndian.Uint32(d[0x28:])) << 32
	}

	byteOff := tableBlock*fs.blockSize + index*int64(fs.inodeSize)
	blk, err := fs.readBlock(byteOff / fs.blockSize)
	if err != nil {
		return nil, err
	}
	off := byteOff % fs.blockSize
	return blk[off : off+int64(fs.inodeSize)], nil
}

type extent struct {
	fileBlock uint32 // logical block within the file
	physBlock int64  // 0 = unwritten (reads as zeros)
	count     uint32
}

// extents walks an inode's extent tree, depth-first, returning leaf extents.
func (fs *FS) extents(ino []byte) ([]extent, error) {
	flags := binary.LittleEndian.Uint32(ino[0x20:])
	if flags&inodeFlagInlineData != 0 {
		return nil, errors.New("ext4: inline_data inode")
	}
	if flags&inodeFlagExtents == 0 {
		return nil, errors.New("ext4: non-extent (block-map) inode")
	}
	return fs.extentNode(ino[0x28:0x28+60], 0)
}

func (fs *FS) extentNode(node []byte, depth int) ([]extent, error) {
	if depth > 6 {
		return nil, errors.New("ext4: extent tree too deep")
	}
	if binary.LittleEndian.Uint16(node[0:]) != 0xF30A {
		return nil, errors.New("ext4: bad extent node magic")
	}
	entries := int(binary.LittleEndian.Uint16(node[2:]))
	nodeDepth := binary.LittleEndian.Uint16(node[6:])
	if 12+entries*12 > len(node) {
		return nil, errors.New("ext4: extent node overflow")
	}
	var out []extent
	for i := 0; i < entries; i++ {
		e := node[12+i*12:]
		if nodeDepth == 0 {
			ln := binary.LittleEndian.Uint16(e[4:])
			phys := int64(binary.LittleEndian.Uint32(e[8:])) |
				int64(binary.LittleEndian.Uint16(e[6:]))<<32
			ext := extent{
				fileBlock: binary.LittleEndian.Uint32(e[0:]),
				physBlock: phys,
				count:     uint32(ln),
			}
			if ln > 32768 { // unwritten extent: allocated but zero-reading
				ext.count = uint32(ln) - 32768
				ext.physBlock = 0
			}
			out = append(out, ext)
			continue
		}
		leaf := int64(binary.LittleEndian.Uint32(e[4:])) |
			int64(binary.LittleEndian.Uint16(e[8:]))<<32
		blk, err := fs.readBlock(leaf)
		if err != nil {
			return nil, err
		}
		sub, err := fs.extentNode(blk, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// readInodeFile materializes a whole file. Holes and unwritten extents read
// as zeros, exactly like the kernel.
func (fs *FS) readInodeFile(ino []byte) ([]byte, error) {
	size := int64(binary.LittleEndian.Uint32(ino[0x4:])) |
		int64(binary.LittleEndian.Uint32(ino[0x6C:]))<<32
	exts, err := fs.extents(ino)
	if err != nil {
		return nil, err
	}
	out := make([]byte, size)
	for _, e := range exts {
		if e.physBlock == 0 {
			continue
		}
		for b := uint32(0); b < e.count; b++ {
			dst := (int64(e.fileBlock) + int64(b)) * fs.blockSize
			if dst >= size {
				break
			}
			blk, err := fs.readBlock(e.physBlock + int64(b))
			if err != nil {
				return nil, err
			}
			copy(out[dst:], blk) // copy stops at len(out)
		}
	}
	return out, nil
}

// lookup resolves a slash-separated path to an inode number.
func (fs *FS) lookup(path string) (uint32, error) {
	ino := uint32(rootIno)
	for _, comp := range strings.Split(strings.Trim(path, "/"), "/") {
		if comp == "" || comp == "." {
			continue
		}
		b, err := fs.inodeBytes(ino)
		if err != nil {
			return 0, err
		}
		if mode := binary.LittleEndian.Uint16(b[0:]); mode&0xF000 != 0x4000 {
			return 0, fmt.Errorf("%w: %q is not a directory", ErrNotExist, comp)
		}
		data, err := fs.readInodeFile(b)
		if err != nil {
			return 0, err
		}
		next, err := findDirEntry(data, fs.blockSize, comp)
		if err != nil {
			return 0, fmt.Errorf("%w: component %q", err, comp)
		}
		ino = next
	}
	return ino, nil
}

// findDirEntry linear-scans directory content. Works for htree directories
// too: interior index blocks masquerade as empty dirents (inode 0), and
// leaf blocks hold ordinary entries.
func findDirEntry(dir []byte, blockSize int64, name string) (uint32, error) {
	for base := int64(0); base < int64(len(dir)); base += blockSize {
		off := base
		for off < base+blockSize && off+8 <= int64(len(dir)) {
			e := dir[off:]
			ino := binary.LittleEndian.Uint32(e[0:])
			recLen := int64(binary.LittleEndian.Uint16(e[4:]))
			nameLen := int(e[6])
			if recLen < 8 || off+recLen > base+blockSize {
				break // corrupt or checksum-tail entry: next block
			}
			if ino != 0 && 8+nameLen <= int(recLen) &&
				string(e[8:8+nameLen]) == name {
				return ino, nil
			}
			off += recLen
		}
	}
	return 0, ErrNotExist
}

// ReadFile returns the content of the regular file at path ("/dir/app.db").
func (fs *FS) ReadFile(path string) ([]byte, error) {
	ino, err := fs.lookup(path)
	if err != nil {
		return nil, err
	}
	b, err := fs.inodeBytes(ino)
	if err != nil {
		return nil, err
	}
	if mode := binary.LittleEndian.Uint16(b[0:]); mode&0xF000 != 0x8000 {
		return nil, fmt.Errorf("%w: %q is not a regular file", ErrNotExist, path)
	}
	return fs.readInodeFile(b)
}

// FileSize returns the size of the file at path without reading its data.
func (fs *FS) FileSize(path string) (int64, error) {
	ino, err := fs.lookup(path)
	if err != nil {
		return 0, err
	}
	b, err := fs.inodeBytes(ino)
	if err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(b[0x4:])) |
		int64(binary.LittleEndian.Uint32(b[0x6C:]))<<32, nil
}
