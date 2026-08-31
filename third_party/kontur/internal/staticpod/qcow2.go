package staticpod

import (
	"encoding/binary"
	"fmt"
	"os"
)

// qcow2 layout constants. Every overlay this package creates uses 64KiB
// clusters and 16-bit refcount entries (qemu-img's own defaults), and is
// laid out as exactly four clusters:
//
//	cluster 0: header, the backing-file-format extension, and the
//	           backing file name
//	cluster 1: refcount table (one entry, pointing at cluster 2)
//	cluster 2: refcount block (refcounts for clusters 0-3, plus its own
//	           L1 table cluster(s))
//	cluster 3+: L1 table, all-zero
//
// Leaving the L1 table all-zero is what makes this an empty overlay: a
// zero L1 entry means "no L2 table allocated for this range", which
// qcow2 readers (including cloud-hypervisor's) treat as "read the
// backing file instead" -- so the overlay starts out describing zero
// bytes of its own regardless of the virtual disk size, and only grows
// as the guest actually writes to it.
const (
	qcow2ClusterBits = 16
	qcow2ClusterSize = 1 << qcow2ClusterBits // 64 KiB

	qcow2Magic   = 0x514649fb // "QFI\xfb"
	qcow2Version = 3

	// qcow2RefcountOrder selects a 16-bit refcount entry (1 <<
	// qcow2RefcountOrder bits each), giving a single refcount block
	// cluster room for 32768 entries -- vastly more than the handful of
	// clusters a freshly created overlay ever needs to track.
	qcow2RefcountOrder      = 4
	qcow2RefcountEntryBits  = 1 << qcow2RefcountOrder
	qcow2EntriesPerRefBlock = qcow2ClusterSize * 8 / qcow2RefcountEntryBits

	// qcow2HeaderSize is the fixed set of v3 header fields, from magic
	// through header_length itself.
	qcow2HeaderSize = 104

	// qcow2CompressionTypeAndPaddingSize covers the one-byte
	// compression_type field at offset 104 and the 7 bytes of padding
	// after it, which every v3 reader expects to find there whenever
	// header_length is greater than qcow2HeaderSize -- regardless of
	// whether the image ever actually uses compressed clusters (this one
	// never does).
	qcow2CompressionTypeAndPaddingSize = 8

	// qcow2HeaderLength is the value stored in the header's own
	// header_length field: qcow2HeaderSize plus room for
	// compression_type and its padding, so that the header extension
	// area below starts at a clean offset instead of colliding with
	// compression_type. (An earlier version of this code set
	// header_length to exactly qcow2HeaderSize -- 104 -- leaving no room
	// for compression_type, and a backing-file-format extension's own
	// magic byte landed right on top of it; readers parsed that byte as
	// an unrecognized compression type and rejected the image outright.
	// Reserving these 8 bytes is what makes it safe to add the extension
	// below.)
	qcow2HeaderLength = qcow2HeaderSize + qcow2CompressionTypeAndPaddingSize

	// qcow2ExtBackingFormatMagic identifies a header extension that
	// pins the backing file's format, rather than leaving it to be
	// probed from the backing file's own content. Leaving it unpinned
	// is what used to happen here, on the assumption that qcow2 readers
	// fall back to content-based probing the same way qcow2 v2 always
	// did -- but cloud-hypervisor's reader doesn't: given a raw backing
	// file and no pinned format, it instead trips its own
	// backing-chain-depth guard ("Maximum disk nesting depth exceeded"),
	// apparently from repeatedly trying to reinterpret the raw image's
	// bytes as further qcow2/backing headers. Every overlay
	// writeQcow2Overlay creates is backed by a raw source disk image
	// (see PrepareWritableDisk), so qcow2BackingFormat is always "raw".
	qcow2ExtBackingFormatMagic = 0xE2792ACA
	qcow2BackingFormat         = "raw"
	// qcow2BackingFormatExtSize is the backing-file-format extension's
	// total on-disk size: an 8-byte extension header (magic + data
	// length) plus qcow2BackingFormat's data padded up to the next
	// 8-byte boundary, as every qcow2 header extension's data must be
	// (3 bytes of "raw" padded to 8).
	qcow2BackingFormatExtSize = 8 + 8
	// qcow2ExtEndSize is the magic-0/length-0 entry that terminates the
	// header extension area.
	qcow2ExtEndSize = 8

	qcow2L2EntriesPerTable = qcow2ClusterSize / 8
	// qcow2L1CoverageBytes is how much of the virtual disk one L1 entry
	// (i.e. one L2 table) covers: 512MiB with 64KiB clusters.
	qcow2L1CoverageBytes = int64(qcow2ClusterSize) * int64(qcow2L2EntriesPerTable)
)

// writeQcow2Overlay creates a new qcow2 image at dest: a thin,
// copy-on-write overlay presenting sizeBytes of virtual disk, with no
// data clusters of its own -- every read falls through to backingFile
// until the guest writes, at which point cloud-hypervisor's own qcow2
// implementation allocates clusters lazily as needed. Creating the
// overlay is therefore cheap and constant-size regardless of
// sizeBytes, unlike copying backingFile's contents byte for byte.
//
// backingFile is stored verbatim as the qcow2 backing file path, so it
// must be resolvable by whatever process later opens dest -- i.e. the
// path as seen inside the VM container (under ImagesMountPath), not a
// host path.
//
// dest must not already exist; writeQcow2Overlay fails otherwise rather
// than overwriting it, the same way the copy it replaces never
// clobbered an existing writable disk.
func writeQcow2Overlay(dest, backingFile string, sizeBytes int64) (err error) {
	l1Size := (sizeBytes + qcow2L1CoverageBytes - 1) / qcow2L1CoverageBytes
	if l1Size < 1 {
		l1Size = 1
	}
	l1TableClusters := (l1Size*8 + qcow2ClusterSize - 1) / qcow2ClusterSize

	const (
		headerCluster        = 0
		refcountTableCluster = 1
		refcountBlockCluster = 2
		l1TableStartCluster  = 3
	)
	totalClusters := int64(l1TableStartCluster) + l1TableClusters
	if totalClusters > qcow2EntriesPerRefBlock {
		return fmt.Errorf("virtual disk size %d bytes needs an L1 table too large for a single-cluster refcount block", sizeBytes)
	}

	buf := make([]byte, totalClusters*qcow2ClusterSize)

	// --- cluster 0: header + extension area + backing file name ---
	extensionAreaOffset := qcow2HeaderLength
	backingFileOffset := uint64(extensionAreaOffset + qcow2BackingFormatExtSize + qcow2ExtEndSize)

	h := buf[:qcow2HeaderSize]
	binary.BigEndian.PutUint32(h[0:4], qcow2Magic)
	binary.BigEndian.PutUint32(h[4:8], qcow2Version)
	binary.BigEndian.PutUint64(h[8:16], backingFileOffset)
	binary.BigEndian.PutUint32(h[16:20], uint32(len(backingFile)))
	binary.BigEndian.PutUint32(h[20:24], qcow2ClusterBits)
	binary.BigEndian.PutUint64(h[24:32], uint64(sizeBytes))
	binary.BigEndian.PutUint32(h[32:36], 0) // crypt_method: none
	binary.BigEndian.PutUint32(h[36:40], uint32(l1Size))
	binary.BigEndian.PutUint64(h[40:48], uint64(l1TableStartCluster*qcow2ClusterSize))
	binary.BigEndian.PutUint64(h[48:56], uint64(refcountTableCluster*qcow2ClusterSize))
	binary.BigEndian.PutUint32(h[56:60], 1) // refcount_table_clusters
	binary.BigEndian.PutUint32(h[60:64], 0) // nb_snapshots
	binary.BigEndian.PutUint64(h[64:72], 0) // snapshots_offset
	binary.BigEndian.PutUint64(h[72:80], 0) // incompatible_features
	binary.BigEndian.PutUint64(h[80:88], 0) // compatible_features
	binary.BigEndian.PutUint64(h[88:96], 0) // autoclear_features
	binary.BigEndian.PutUint32(h[96:100], qcow2RefcountOrder)
	binary.BigEndian.PutUint32(h[100:104], uint32(qcow2HeaderLength))

	// --- byte 104: compression_type, and 7 bytes padding after it: left
	// zero, since writeQcow2Overlay never produces compressed clusters ---

	// --- header extension: backing file format, pinned to "raw" (see
	// qcow2ExtBackingFormatMagic's doc comment) ---
	ext := buf[extensionAreaOffset : extensionAreaOffset+qcow2BackingFormatExtSize]
	binary.BigEndian.PutUint32(ext[0:4], qcow2ExtBackingFormatMagic)
	binary.BigEndian.PutUint32(ext[4:8], uint32(len(qcow2BackingFormat)))
	copy(ext[8:8+len(qcow2BackingFormat)], qcow2BackingFormat)
	// ext[8+len(qcow2BackingFormat):] is this extension's padding, left zero

	// --- end-of-extensions marker (magic 0, length 0): already zero from make() ---

	copy(buf[backingFileOffset:], backingFile)

	// --- cluster 1: refcount table, one entry pointing at cluster 2 ---
	rt := buf[refcountTableCluster*qcow2ClusterSize:]
	binary.BigEndian.PutUint64(rt[0:8], uint64(refcountBlockCluster*qcow2ClusterSize))

	// --- cluster 2: refcount block, refcount 1 for every cluster this overlay starts with ---
	rb := buf[refcountBlockCluster*qcow2ClusterSize:]
	for c := int64(0); c < totalClusters; c++ {
		binary.BigEndian.PutUint16(rb[c*2:c*2+2], 1)
	}

	// L1 table clusters (from l1TableStartCluster) are left all-zero: no
	// L2 table allocated anywhere, so every read falls through to
	// backingFile.

	out, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(dest)
		}
	}()
	_, err = out.Write(buf)
	return err
}
