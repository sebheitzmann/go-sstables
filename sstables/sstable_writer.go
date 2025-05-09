package sstables

import (
	"errors"
	"fmt"
	"hash/crc64"
	"hash/fnv"
	"os"
	"path/filepath"

	"github.com/steakknife/bloomfilter"
	"github.com/thomasjungblut/go-sstables/recordio"
	rProto "github.com/thomasjungblut/go-sstables/recordio/proto"
	"github.com/thomasjungblut/go-sstables/skiplist"
	sProto "github.com/thomasjungblut/go-sstables/sstables/proto"
	"google.golang.org/protobuf/proto"
)

type SSTableStreamWriter struct {
	opts *SSTableWriterOptions

	indexFilePath string
	dataFilePath  string
	metaFilePath  string

	indexWriter  rProto.WriterI
	dataWriter   recordio.WriterI
	metaDataFile *os.File

	bloomFilter *bloomfilter.Filter
	metaData    *sProto.MetaData

	lastKey []byte
}

func (writer *SSTableStreamWriter) Open() error {
	writer.indexFilePath = filepath.Join(writer.opts.basePath, IndexFileName)
	iWriter, err := rProto.NewWriter(
		rProto.Path(writer.indexFilePath),
		rProto.CompressionType(writer.opts.indexCompressionType),
		rProto.WriteBufferSizeBytes(writer.opts.writeBufferSizeBytes))
	if err != nil {
		return fmt.Errorf("error while creating index writer in '%s': %w", writer.opts.basePath, err)
	}
	writer.indexWriter = iWriter

	err = writer.indexWriter.Open()
	if err != nil {
		return fmt.Errorf("error while opening index writer in '%s': %w", writer.opts.basePath, err)
	}

	writer.dataFilePath = filepath.Join(writer.opts.basePath, DataFileName)
	dWriter, err := recordio.NewFileWriter(
		recordio.Path(writer.dataFilePath),
		recordio.CompressionType(writer.opts.dataCompressionType),
		recordio.BufferSizeBytes(writer.opts.writeBufferSizeBytes))
	if err != nil {
		return fmt.Errorf("error while creating data writer in '%s': %w", writer.opts.basePath, err)
	}

	// TODO(thomas): if any of these open fails, we should try to at least close the ones we already have opened
	writer.dataWriter = dWriter
	err = writer.dataWriter.Open()
	if err != nil {
		return fmt.Errorf("error while opening data writer in '%s': %w", writer.opts.basePath, err)
	}

	writer.metaFilePath = filepath.Join(writer.opts.basePath, MetaFileName)
	metaFile, err := os.OpenFile(writer.metaFilePath, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("error while opening metadata file in '%s': %w", writer.opts.basePath, err)
	}
	writer.metaDataFile = metaFile
	writer.metaData = &sProto.MetaData{
		Version: Version,
	}

	if writer.opts.enableBloomFilter {
		bf, err := bloomfilter.NewOptimal(writer.opts.bloomExpectedNumberOfElements, writer.opts.bloomFpProbability)
		if err != nil {
			return fmt.Errorf("error while creating bloomfilter in '%s': %w", writer.opts.basePath, err)
		}
		writer.bloomFilter = bf
	}

	return nil
}

func (writer *SSTableStreamWriter) WriteNext(key []byte, value []byte) error {
	if writer.lastKey != nil {
		cmpResult := writer.opts.keyComparator.Compare(writer.lastKey, key)
		if cmpResult == 0 {
			return fmt.Errorf("sstables.WriteNext '%s': the same key cannot be written more than once", writer.opts.basePath)
		} else if cmpResult > 0 {
			return fmt.Errorf("sstables.WriteNext '%s': non-ascending key cannot be written", writer.opts.basePath)
		}

		// the size of the key may be variable, that's why we might allocate a new buffer for the last key
		if len(writer.lastKey) != len(key) {
			writer.lastKey = make([]byte, len(key))
		}
	} else {
		if writer.metaData == nil {
			return fmt.Errorf("sstables.writeNext '%s': no metadata available to write into, table might not be opened yet", writer.opts.basePath)
		}

		writer.metaData.MinKey = make([]byte, len(key))
		writer.lastKey = make([]byte, len(key))
		copy(writer.metaData.MinKey, key)
	}

	copy(writer.lastKey, key)

	if writer.opts.enableBloomFilter {
		fnvHash := fnv.New64()
		_, _ = fnvHash.Write(key)
		writer.bloomFilter.Add(fnvHash)
	}

	crc := crc64.New(crc64.MakeTable(crc64.ISO))
	_, err := crc.Write(value)
	if err != nil {
		return fmt.Errorf("error while writing crc64 hash in '%s': %w", writer.opts.basePath, err)
	}

	preWriteOffset := writer.dataWriter.Size()
	recordOffset, err := writer.dataWriter.Write(value)
	if err != nil {
		return fmt.Errorf("error writeNext data writer error in '%s': %w", writer.opts.basePath, err)
	}

	_, err = writer.indexWriter.Write(&sProto.IndexEntry{Key: key, ValueOffset: recordOffset, Checksum: crc.Sum64()})
	if err != nil {
		// in case of failures we need to try to rewind the data writer's offset to preWriteOffset
		seekErr := writer.dataWriter.Seek(preWriteOffset)
		return fmt.Errorf("error writeNext index writer/seeker error in '%s': %w", writer.opts.basePath, errors.Join(err, seekErr))
	}

	writer.metaData.NumRecords += 1
	if value == nil {
		writer.metaData.NullValues += 1
	}

	return nil
}

func (writer *SSTableStreamWriter) Close() (err error) {
	err = errors.Join(writer.indexWriter.Close(), writer.dataWriter.Close())

	if writer.opts.enableBloomFilter && writer.bloomFilter != nil {
		_, bErr := writer.bloomFilter.WriteFile(filepath.Join(writer.opts.basePath, BloomFileName))
		if bErr != nil {
			err = errors.Join(err, fmt.Errorf("error in writing bloom filter  in '%s': %w", writer.opts.basePath, bErr))
		}
	}

	if writer.metaData != nil && writer.metaDataFile != nil {
		defer func() {
			err = errors.Join(err, writer.metaDataFile.Close())
		}()

		writer.metaData.MaxKey = writer.lastKey
		writer.metaData.DataBytes = writer.dataWriter.Size()
		writer.metaData.IndexBytes = writer.indexWriter.Size()
		writer.metaData.TotalBytes = writer.metaData.DataBytes + writer.metaData.IndexBytes
		bytes, mErr := proto.Marshal(writer.metaData)
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("error in serializing metadata in '%s': %w", writer.opts.basePath, mErr))
		}

		_, wErr := writer.metaDataFile.Write(bytes)
		if wErr != nil {
			return errors.Join(err, fmt.Errorf("error in writing metadata in '%s': %w", writer.opts.basePath, wErr))
		}
	}

	return err
}

type SSTableSimpleWriter struct {
	streamWriter *SSTableStreamWriter
}

func (writer *SSTableSimpleWriter) WriteSkipListMap(skipListMap skiplist.MapI[[]byte, []byte]) (err error) {
	err = writer.streamWriter.Open()
	if err != nil {
		return err
	}

	defer func() {
		err = errors.Join(err, writer.streamWriter.Close())
	}()

	it, _ := skipListMap.Iterator()
	for {
		k, v, err := it.Next()
		if errors.Is(err, skiplist.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("error in getting next skiplist record in '%s': %w", writer.streamWriter.opts.basePath, err)
		}

		err = writer.streamWriter.WriteNext(k, v)
		if err != nil {
			return fmt.Errorf("error in writing skiplist record in '%s': %w", writer.streamWriter.opts.basePath, err)
		}
	}

	return nil
}

// NewSSTableStreamWriter creates a new streamed writer, the minimum options required are the base path and the comparator:
// > sstables.NewSSTableStreamWriter(sstables.WriteBasePath("some_existing_folder"), sstables.WithKeyComparator(some_comparator))
func NewSSTableStreamWriter(writerOptions ...WriterOption) (*SSTableStreamWriter, error) {
	opts := &SSTableWriterOptions{
		basePath:                      "",
		enableBloomFilter:             true,
		indexCompressionType:          recordio.CompressionTypeNone,
		dataCompressionType:           recordio.CompressionTypeSnappy,
		bloomFpProbability:            0.01,
		bloomExpectedNumberOfElements: 1000,
		writeBufferSizeBytes:          1024 * 1024 * 4,
		keyComparator:                 nil,
	}

	for _, writeOption := range writerOptions {
		writeOption(opts)
	}

	if opts.basePath == "" {
		return nil, errors.New("basePath was not supplied")
	}

	if opts.keyComparator == nil {
		return nil, errors.New("no key comparator supplied")
	}

	if opts.bloomExpectedNumberOfElements <= 0 {
		return nil, fmt.Errorf("unexpected number of bloom filter elements, was: %d",
			opts.bloomExpectedNumberOfElements)
	}

	return &SSTableStreamWriter{opts: opts}, nil
}

func NewSSTableSimpleWriter(writerOptions ...WriterOption) (*SSTableSimpleWriter, error) {
	writerOptions = append(writerOptions, WriteBufferSizeBytes(4096))
	writer, err := NewSSTableStreamWriter(writerOptions...)
	if err != nil {
		return nil, err
	}
	return &SSTableSimpleWriter{streamWriter: writer}, nil
}

// options

type SSTableWriterOptions struct {
	basePath                      string
	indexCompressionType          int
	dataCompressionType           int
	enableBloomFilter             bool
	bloomExpectedNumberOfElements uint64
	bloomFpProbability            float64
	writeBufferSizeBytes          int
	keyComparator                 skiplist.Comparator[[]byte]
}

type WriterOption func(*SSTableWriterOptions)

func WriteBasePath(p string) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.basePath = p
	}
}

func IndexCompressionType(p int) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.indexCompressionType = p
	}
}

func DataCompressionType(p int) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.dataCompressionType = p
	}
}

func EnableBloomFilter() WriterOption {
	return func(args *SSTableWriterOptions) {
		args.enableBloomFilter = true
	}
}

func BloomExpectedNumberOfElements(n uint64) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.bloomExpectedNumberOfElements = n
	}
}

func BloomFalsePositiveProbability(fpProbability float64) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.bloomFpProbability = fpProbability
	}
}

func WriteBufferSizeBytes(bufSizeBytes int) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.writeBufferSizeBytes = bufSizeBytes
	}
}

func WithKeyComparator(cmp skiplist.Comparator[[]byte]) WriterOption {
	return func(args *SSTableWriterOptions) {
		args.keyComparator = cmp
	}
}
