package benchmark

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/thomasjungblut/go-sstables/memstore"
	"github.com/thomasjungblut/go-sstables/skiplist"
	"github.com/thomasjungblut/go-sstables/sstables"
)

func BenchmarkSSTableRead(b *testing.B) {
	benchmarks := []struct {
		name         string
		memstoreSize int
	}{
		{"32mb", 1024 * 1024 * 32},
		{"64mb", 1024 * 1024 * 64},
		{"128mb", 1024 * 1024 * 128},
		{"256mb", 1024 * 1024 * 256},
		{"512mb", 1024 * 1024 * 512},
		{"1024mb", 1024 * 1024 * 1024},
		{"2048mb", 1024 * 1024 * 1024 * 2},
		{"4096mb", 1024 * 1024 * 1024 * 4},
		{"8192mb", 1024 * 1024 * 1024 * 8},
	}

	cmp := skiplist.BytesComparator{}
	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			tmpDir, err := os.MkdirTemp("", "sstable_BenchRead_"+bm.name)
			assert.Nil(b, err)
			defer func() { assert.Nil(b, os.RemoveAll(tmpDir)) }()

			writeSSTableWithSize(b, bm.memstoreSize, tmpDir, cmp, false)

			b.ResetTimer()
			fullScanTable(b, tmpDir, cmp, nil, nil)
		})
	}
}

func BenchmarkSSTableReadIndexTypes(b *testing.B) {
	sizeTwoGigs := 1024 * 1024 * 1024 * 2
	cmp := skiplist.BytesComparator{}
	sha1keymapper := sstables.Byte20KeyMapper[[20]byte]{}
	benchmarks := []struct {
		name       string
		loader     sstables.IndexLoader
		dataloader sstables.DataLoader
	}{
		{"map",
			&sstables.SortedMapIndexLoader[[20]byte]{
				ReadBufferSize: 4096,
				Binary:         false,
				Mapper:         sha1keymapper,
			},
			nil},
		{"mapBinary",
			&sstables.SortedMapIndexLoader[[20]byte]{
				ReadBufferSize: 4096,
				Binary:         true,
				Mapper:         sha1keymapper,
			},
			sstables.NewBinaryDataLoader()},
		{"skiplist", &sstables.SkipListIndexLoader{
			KeyComparator:  cmp,
			ReadBufferSize: 4096,
		}, nil},
		{"slice", &sstables.SliceKeyIndexLoader{ReadBufferSize: 4096}, nil},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			tmpDir, err := os.MkdirTemp("", "sstable_BenchReadIndexLoad_"+bm.name)
			assert.Nil(b, err)
			defer func() { assert.Nil(b, os.RemoveAll(tmpDir)) }()

			writeSSTableWithSize(b, sizeTwoGigs, tmpDir, cmp, bm.dataloader != nil)
			b.ResetTimer()
			fullScanTable(b, tmpDir, cmp, bm.loader, bm.dataloader)
		})
	}
}

func writeSSTableWithSize(b *testing.B, sizeBytes int, tmpDir string, cmp skiplist.BytesComparator, binaryWriter bool) {
	mStore := memstore.NewMemStore()
	bytes := randomRecordOfSize(1024)

	i := 0
	for mStore.EstimatedSizeInBytes() < uint64(sizeBytes) {
		kx := make([]byte, 4)
		binary.BigEndian.PutUint32(kx, uint32(i))
		hash := sha1.New()
		hash.Write(kx)

		k := hash.Sum([]byte{})
		assert.Nil(b, mStore.Add(k, bytes))
		i++
	}
	if binaryWriter {
		assert.Nil(b, flushMemstoreBinary(mStore, sstables.WriteBasePath(tmpDir), sstables.WithKeyComparator(cmp)))
	} else {
		assert.Nil(b, mStore.Flush(sstables.WriteBasePath(tmpDir), sstables.WithKeyComparator(cmp)))
	}
}

func flushMemstoreBinary(m memstore.MemStoreI, writerOptions ...sstables.WriterOption) (err error) {
	writer, err := sstables.NewSSTableStreamWriterBinary(writerOptions...)
	if err != nil {
		return err
	}

	err = writer.Open()
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, writer.Close())
	}()

	it := m.SStableIterator()
	for {
		k, v, err := it.Next()
		if errors.Is(err, sstables.Done) {
			break
		}
		if err != nil {
			return err
		}
		if err := writer.WriteNext(k, v); err != nil {
			return err
		}
	}

	return nil
}

func fullScanTable(b *testing.B, tmpDir string, cmp skiplist.Comparator[[]byte], loader sstables.IndexLoader, dataloader sstables.DataLoader) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loadStart := time.Now()
		opts := []sstables.ReadOption{
			sstables.ReadBasePath(tmpDir),
			sstables.ReadWithKeyComparator(cmp),
			sstables.SkipHashCheckOnLoad(),
		}
		if loader != nil {
			opts = append(opts, sstables.ReadIndexLoader(loader))
		}
		if dataloader != nil {
			opts = append(opts, sstables.WithDataLoader(dataloader))
		}
		reader, err := sstables.NewSSTableReader(opts...)
		assert.NoError(b, err)
		loadEnd := time.Now().Sub(loadStart)
		b.ReportMetric(float64(loadEnd.Milliseconds()), "load_time_ms")
		b.ReportMetric(float64(loadEnd.Nanoseconds())/float64(reader.MetaData().NumRecords), "load_time_ns/record")
		b.ReportMetric(float64(reader.MetaData().IndexBytes), "index_bytes")
		b.ReportMetric(float64(reader.MetaData().IndexBytes)/1024/1024/loadEnd.Seconds(), "load_bandwidth_mb/s")

		defer func() {
			assert.Nil(b, reader.Close())
		}()

		assert.Nil(b, err)
		scanStart := time.Now()
		scanner, err := reader.Scan()
		assert.Nil(b, err)
		i := uint64(0)
		for {
			_, _, err := scanner.Next()
			if errors.Is(err, sstables.Done) {
				break
			}
			i++
		}
		if reader.MetaData().NumRecords != i {
			b.Fail()
		}
		scanEnd := time.Now().Sub(scanStart)
		b.SetBytes(int64(reader.MetaData().TotalBytes))
		b.ReportMetric(float64(scanEnd.Milliseconds()), "scan_time_ms")
		b.ReportMetric(float64(scanEnd.Nanoseconds())/float64(i), "scan_time_ns/record")
		b.ReportMetric(float64(i), "num_records")
		b.ReportMetric(float64(reader.MetaData().DataBytes)/1024/1024/scanEnd.Seconds(), "scan_bandwidth_mb/s")

		// call get for all keys
		kx := make([]byte, 4)
		hash := sha1.New()
		getStart := time.Now()
		for i := range reader.MetaData().NumRecords {
			binary.BigEndian.PutUint32(kx, uint32(i))
			hash.Reset()
			hash.Write(kx)
			k := hash.Sum(nil)

			_, err := reader.Get(k)
			if err != nil {
				b.Fail()
			}
		}
		getEnd := time.Now().Sub(getStart)
		b.ReportMetric(float64(getEnd.Milliseconds()), "get_time_ms")
		b.ReportMetric(float64(getEnd.Nanoseconds())/float64(i), "get_time_ns/record")

	}
}
