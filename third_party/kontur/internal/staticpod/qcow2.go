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
//	cluster 0: header and the backing file name
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

	// qcow2HeaderSize is the fixed v3 header, with no room left for
	// extensions: header_length is set to exactly this, so the backing
	// file name starts immediately after it. This deliberately leaves
	// out header extensions altogether (in particular, an explicit
	// backing-file-format one) -- QEMU (and, per its docs, qcow2 readers
	// generally) only look for a "compression_type" byte at offset 104
	// when header_length is greater than 104, so growing the header at
	// all to fit an extension risks that byte being misread as one
	// (confirmed by hand: a backing-format extension's own magic byte
	// landed on offset 104 and was rejected as an "unknown compression
	// type"). Leaving backing file format unset means it's probed from
	// the backing file's own content instead of pinned, same as qcow2
	// v2 (and v3 without -F) has always done.
	qcow2HeaderSize = 104

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

	// --- cluster 0: header + backing file name (no extensions -- see qcow2HeaderSize) ---
	headerLength := uint32(qcow2HeaderSize)
	backingFileOffset := uint64(headerLength)

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
	binary.BigEndian.PutUint32(h[100:104], headerLength)

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
