// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zip

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"log"
)

// Writer implements a zip file writer.
type Writer struct {
	cw          *countWriter
	dir         []*header
	last        *fileWriter
	closed      bool
	compressors map[uint16]Compressor
	names       map[string]int // filename -> index in dir slice.
}

type header struct {
	*FileHeader
	offset uint64
}

// NewWriter returns a new Writer writing a zip file to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{cw: &countWriter{w: bufio.NewWriter(w)}}
}

// SetOffset sets the offset of the beginning of the zip data within the
// underlying writer. It should be used when the zip data is appended to an
// existing file, such as a binary executable.
// It must be called before any data is written.
func (w *Writer) SetOffset(n int64) {
	if w.cw.count != 0 {
		panic("zip: SetOffset called after data was written")
	}
	w.cw.count = n
}

func newAppendingWriter(r *Reader, fw io.Writer, skipManifest bool) *Writer {
	w := &Writer{
		cw: &countWriter{
			w:     bufio.NewWriter(fw),
			count: r.AppendOffset(),
		},
		dir:   make([]*header, 0, len(r.File)*3/2),
		names: make(map[string]int),
	}
	for _, f := range r.File {
		if skipManifest && f.Name == ANDROIDMANIFEST {
			continue
		}
		h := &header{
			FileHeader: &f.FileHeader,
			offset:     uint64(f.headerOffset),
		}
		w.dir = append(w.dir, h)
		w.names[f.Name] = len(w.dir) - 1
	}
	return w
}

// Flush flushes any buffered data to the underlying writer.
// Calling Flush is not normally necessary; calling Close is sufficient.
func (w *Writer) Flush() error {
	return w.cw.w.(*bufio.Writer).Flush()
}

// Close finishes writing the zip file by writing the central directory.
// It does not (and can not) close the underlying writer.
func (w *Writer) Close() error {
	if w.last != nil && !w.last.closed {
		if err := w.last.close(); err != nil {
			return err
		}
		w.last = nil
	}
	if w.closed {
		return errors.New("zip: writer closed twice")
	}
	w.closed = true

	// write central directory
	start := w.cw.count
	records := uint64(0)
	for _, h := range w.dir {
		if h.FileHeader == nil {
			// This entry has been superceded by a later
			// appended entry.
			continue
		}
		records++
		var buf [directoryHeaderLen]byte
		b := writeBuf(buf[:])
		b.uint32(uint32(directoryHeaderSignature))
		b.uint16(h.CreatorVersion)
		b.uint16(h.ReaderVersion)
		b.uint16(h.Flags)
		b.uint16(h.Method)
		b.uint16(h.ModifiedTime)
		b.uint16(h.ModifiedDate)
		b.uint32(h.CRC32)
		if h.isZip64() || h.offset >= uint32max {
			// the file needs a zip64 header. store maxint in both
			// 32 bit size fields (and offset later) to signal that the
			// zip64 extra header should be used.
			b.uint32(uint32max) // compressed size
			b.uint32(uint32max) // uncompressed size

			// append a zip64 extra block to Extra
			var buf [28]byte // 2x uint16 + 3x uint64
			eb := writeBuf(buf[:])
			eb.uint16(zip64ExtraId)
			eb.uint16(24) // size = 3x uint64
			eb.uint64(h.UncompressedSize64)
			eb.uint64(h.CompressedSize64)
			eb.uint64(h.offset)
			h.Extra = append(h.Extra, buf[:]...)
		} else {
			b.uint32(h.CompressedSize)
			b.uint32(h.UncompressedSize)
		}
		b.uint16(uint16(len(h.Name)))
		b.uint16(uint16(len(h.Extra)))
		b.uint16(uint16(len(h.Comment)))
		b = b[4:] // skip disk number start and internal file attr (2x uint16)
		b.uint32(h.ExternalAttrs)
		if h.offset > uint32max {
			b.uint32(uint32max)
		} else {
			b.uint32(uint32(h.offset))
		}
		if _, err := w.cw.Write(buf[:]); err != nil {
			return err
		}
		if _, err := io.WriteString(w.cw, h.Name); err != nil {
			return err
		}
		if _, err := w.cw.Write(h.Extra); err != nil {
			return err
		}
		if _, err := io.WriteString(w.cw, h.Comment); err != nil {
			return err
		}
	}
	end := w.cw.count

	size := uint64(end - start)
	offset := uint64(start)

	if records > uint16max || size > uint32max || offset > uint32max {
		var buf [directory64EndLen + directory64LocLen]byte
		b := writeBuf(buf[:])

		// zip64 end of central directory record
		b.uint32(directory64EndSignature)
		b.uint64(directory64EndLen - 12) // length minus signature (uint32) and length fields (uint64)
		b.uint16(zipVersion45)           // version made by
		b.uint16(zipVersion45)           // version needed to extract
		b.uint32(0)                      // number of this disk
		b.uint32(0)                      // number of the disk with the start of the central directory
		b.uint64(records)                // total number of entries in the central directory on this disk
		b.uint64(records)                // total number of entries in the central directory
		b.uint64(size)                   // size of the central directory
		b.uint64(offset)                 // offset of start of central directory with respect to the starting disk number

		// zip64 end of central directory locator
		b.uint32(directory64LocSignature)
		b.uint32(0)           // number of the disk with the start of the zip64 end of central directory
		b.uint64(uint64(end)) // relative offset of the zip64 end of central directory record
		b.uint32(1)           // total number of disks

		if _, err := w.cw.Write(buf[:]); err != nil {
			return err
		}

		// store max values in the regular end record to signal that
		// that the zip64 values should be used instead
		records = uint16max
		size = uint32max
		offset = uint32max
	}

	// write end record
	var buf [directoryEndLen]byte
	b := writeBuf(buf[:])
	b.uint32(uint32(directoryEndSignature))
	b = b[4:]                 // skip over disk number and first disk number (2x uint16)
	b.uint16(uint16(records)) // number of entries this disk
	b.uint16(uint16(records)) // number of entries total
	b.uint32(uint32(size))    // size of directory
	b.uint32(uint32(offset))  // start of directory
	// skipped size of comment (always zero)
	if _, err := w.cw.Write(buf[:]); err != nil {
		return err
	}

	return w.cw.w.(*bufio.Writer).Flush()
}

// Create adds a file to the zip file using the provided name.
// It returns a Writer to which the file contents should be written.
// The name must be a relative path: it must not start with a drive
// letter (e.g. C:) or leading slash, and only forward slashes are
// allowed.
// The file's contents must be written to the io.Writer before the next
// call to Create, CreateHeader, or Close.
func (w *Writer) Create(name string) (io.Writer, error) {
	header := &FileHeader{
		Name:   name,
		Method: Deflate,
	}
	return w.CreateHeader(header)
}

func (w *Writer) closeLastWriter() error {
	if w.last != nil && !w.last.closed {
		err := w.last.close()
		w.last = nil
		return err
	}
	return nil
}

// CreateHeader adds a file to the zip file using the provided FileHeader
// for the file metadata.
// It returns a Writer to which the file contents should be written.
//
// The file's contents must be written to the io.Writer before the next
// call to Create, CreateHeader, or Close. The provided FileHeader fh
// must not be modified after a call to CreateHeader.
func (w *Writer) CreateHeader(fh *FileHeader) (io.Writer, error) {
	if err := w.closeLastWriter(); err != nil {
		return nil, err
	}
	if len(w.dir) > 0 && w.dir[len(w.dir)-1].FileHeader == fh {
		// See https://golang.org/issue/11144 confusion.
		return nil, errors.New("archive/zip: invalid duplicate FileHeader")
	}
	if i, ok := w.names[fh.Name]; ok {
		// We're appending a file that existed already,
		// so clear out the old entry so that it won't
		// be added to the index.
		w.dir[i].FileHeader = nil
		delete(w.names, fh.Name)
	}

	fh.Flags |= 0x8 // we will write a data descriptor

	fh.CreatorVersion = fh.CreatorVersion&0xff00 | zipVersion20 // preserve compatibility byte
	fh.ReaderVersion = zipVersion20

	fw := &fileWriter{
		zipw:      w.cw,
		compCount: &countWriter{w: w.cw},
		crc32:     crc32.NewIEEE(),
	}
	comp := w.compressor(fh.Method)
	if comp == nil {
		return nil, ErrAlgorithm
	}
	var err error
	fw.comp, err = comp(fw.compCount)
	if err != nil {
		return nil, err
	}
	fw.rawCount = &countWriter{w: fw.comp}

	h := &header{
		FileHeader: fh,
		offset:     uint64(w.cw.count),
	}
	w.dir = append(w.dir, h)
	fw.header = h

	if err := writeHeader(w.cw, fh); err != nil {
		return nil, err
	}

	w.last = fw
	return fw, nil
}

func (w *Writer) PaddingHeader(fh *FileHeader) {
	var alignment = 4
	var padlen int
	if fh.CompressedSize64 != fh.UncompressedSize64 {
		// File is compressed, copy the entry without padding
		log.Printf("--- %s: len %d (compressed)", fh.Name, fh.UncompressedSize64)
	} else {
		// source: https://android.googlesource.com/platform/build.git/+/android-4.2.2_r1/tools/zipalign/ZipAlign.cpp#76
		newOffset := len(fh.Extra) + 4
		padlen = (alignment - (newOffset % alignment)) % alignment
		if padlen > 0 {
			log.Printf(" --- %s: padding success %d bytes", fh.Name, padlen)
		} else {
			log.Printf(" --- %s: need not padding %d bytes", fh.Name, padlen)
		}
	}
	// add padlen number of null bytes to the extra field of the file header
	// in order to align files on 4 bytes
	for i := 0; i < padlen; i++ {
		fh.Extra = append(fh.Extra, '\x00')
	}
}

// Copy copies the file f (obtained from a Reader) into w.
// It copies the compressed form directly.
func (w *Writer) Copy(f *File) error {
	dataOffset, err := f.DataOffset()
	if err != nil {
		return err
	}
	if err := w.closeLastWriter(); err != nil {
		return err
	}

	fh := f.FileHeader
	h := &header{
		FileHeader: &fh,
		offset:     uint64(w.cw.count),
	}
	fh.Flags |= 0x8 // we will write a data descriptor
	w.dir = append(w.dir, h)
	//w.PaddingHeader(&fh)
	if err := writeHeader(w.cw, &fh); err != nil {
		return err
	}

	r := io.NewSectionReader(f.zipr, dataOffset, int64(f.CompressedSize64))
	if _, err := io.Copy(w.cw, r); err != nil {
		return err
	}

	return writeDesc(w.cw, &fh)
}

func writeHeader(w io.Writer, h *FileHeader) error {
	var buf [fileHeaderLen]byte
	b := writeBuf(buf[:])
	b.uint32(uint32(fileHeaderSignature))
	b.uint16(h.ReaderVersion)
	b.uint16(h.Flags)
	b.uint16(h.Method)
	b.uint16(h.ModifiedTime)
	b.uint16(h.ModifiedDate)
	b.uint32(0) // since we are writing a data descriptor crc32,
	b.uint32(0) // compressed size,
	b.uint32(0) // and uncompressed size should be zero
	b.uint16(uint16(len(h.Name)))
	b.uint16(uint16(len(h.Extra)))
	if _, err := w.Write(buf[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, h.Name); err != nil {
		return err
	}
	_, err := w.Write(h.Extra)
	return err
}

// RegisterCompressor registers or overrides a custom compressor for a specific
// method ID. If a compressor for a given method is not found, Writer will
// default to looking up the compressor at the package level.
func (w *Writer) RegisterCompressor(method uint16, comp Compressor) {
	if w.compressors == nil {
		w.compressors = make(map[uint16]Compressor)
	}
	w.compressors[method] = comp
}

func (w *Writer) compressor(method uint16) Compressor {
	comp := w.compressors[method]
	if comp == nil {
		comp = compressor(method)
	}
	return comp
}

type fileWriter struct {
	*header
	zipw      io.Writer
	rawCount  *countWriter
	comp      io.WriteCloser
	compCount *countWriter
	crc32     hash.Hash32
	closed    bool
}

func (w *fileWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("zip: write to closed file")
	}
	w.crc32.Write(p)
	return w.rawCount.Write(p)
}

func (w *fileWriter) close() error {
	if w.closed {
		return errors.New("zip: file closed twice")
	}
	w.closed = true
	if err := w.comp.Close(); err != nil {
		return err
	}

	// update FileHeader
	fh := w.header.FileHeader
	fh.CRC32 = w.crc32.Sum32()
	fh.CompressedSize64 = uint64(w.compCount.count)
	fh.UncompressedSize64 = uint64(w.rawCount.count)

	if fh.isZip64() {
		fh.CompressedSize = uint32max
		fh.UncompressedSize = uint32max
		fh.ReaderVersion = zipVersion45 // requires 4.5 - File uses ZIP64 format extensions
	} else {
		fh.CompressedSize = uint32(fh.CompressedSize64)
		fh.UncompressedSize = uint32(fh.UncompressedSize64)
	}

	return writeDesc(w.zipw, fh)
}

func writeDesc(w io.Writer, fh *FileHeader) error {
	// Write data descriptor. This is more complicated than one would
	// think, see e.g. comments in zipfile.c:putextended() and
	// http://bugs.sun.com/bugdatabase/view_bug.do?bug_id=7073588.
	// The approach here is to write 8 byte sizes if needed without
	// adding a zip64 extra in the local header (too late anyway).
	var buf []byte
	if fh.isZip64() {
		buf = make([]byte, dataDescriptor64Len)
	} else {
		buf = make([]byte, dataDescriptorLen)
	}
	b := writeBuf(buf)
	b.uint32(dataDescriptorSignature) // de-facto standard, required by OS X
	b.uint32(fh.CRC32)
	if fh.isZip64() {
		b.uint64(fh.CompressedSize64)
		b.uint64(fh.UncompressedSize64)
	} else {
		b.uint32(fh.CompressedSize)
		b.uint32(fh.UncompressedSize)
	}
	_, err := w.Write(buf)
	return err
}

type countWriter struct {
	w     io.Writer
	count int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.count += int64(n)
	return n, err
}

type nopCloser struct {
	io.Writer
}

func (w nopCloser) Close() error {
	return nil
}

type writeBuf []byte

func (b *writeBuf) uint16(v uint16) {
	binary.LittleEndian.PutUint16(*b, v)
	*b = (*b)[2:]
}

func (b *writeBuf) uint32(v uint32) {
	binary.LittleEndian.PutUint32(*b, v)
	*b = (*b)[4:]
}

func (b *writeBuf) uint64(v uint64) {
	binary.LittleEndian.PutUint64(*b, v)
	*b = (*b)[8:]
}
