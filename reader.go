// ◄◄◄ gobmp/reader.go ►►►
// Copyright (c) 2012 Jason Summers
// Use of this code is governed by an MIT-style license that can
// be found in the readme.md file.
//
// BMP file decoder
//

// Package gobmp implements a BMP image decoder and encoder.
package gobmp

import "image"
import "image/color"
import "io"
import "fmt"

const (
	bI_RGB       = 0
	bI_RLE8      = 1
	bI_RLE4      = 2
	bI_BITFIELDS = 3
)

type decoder struct {
	r io.Reader

	img_Paletted *image.Paletted // Used if dstHasPalette is true
	img_NRGBA    *image.NRGBA    // Used otherwise

	bfOffBits     uint32
	headerSize    uint32
	width         int
	height        int
	bitCount      int
	biCompression uint32

	srcPalNumEntries    int
	srcPalBytesPerEntry int
	srcPalSizeInBytes   int
	dstPalNumEntries    int
	dstHasPalette       bool
	dstPalette          color.Palette

	hasBitfieldsSegment bool
	bitFieldsSize       int
	isTopDown           bool
}

// An UnsupportedError reports that the input uses a valid but unimplemented
// BMP feature.
type UnsupportedError string

func (e UnsupportedError) Error() string { return "bmp: unsupported feature: " + string(e) }

// A FormatError reports that the input is not a valid BMP file.
type FormatError string

func (e FormatError) Error() string { return "bmp: invalid format: " + string(e) }

func getWORD(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8
}
func getDWORD(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func decodeRow_1(d *decoder, buf []byte, j int) error {
	for i := 0; i < d.width; i++ {
		var v byte
		if buf[i/8]&(1<<uint(7-i%8)) == 0 {
			v = 0
		} else {
			v = 1
		}
		if int(v) >= d.dstPalNumEntries {
			return FormatError("palette index out of range")
		}
		d.img_Paletted.Pix[j*d.img_Paletted.Stride+i] = v
	}
	return nil
}

func decodeRow_4(d *decoder, buf []byte, j int) error {
	for i := 0; i < d.width; i++ {
		var v byte
		if i&0x01 == 0 {
			v = buf[i/2] >> 4
		} else {
			v = buf[i/2] & 0x0f
		}
		if int(v) >= d.dstPalNumEntries {
			return FormatError("palette index out of range")
		}
		d.img_Paletted.Pix[j*d.img_Paletted.Stride+i] = v
	}
	return nil
}

func decodeRow_8(d *decoder, buf []byte, j int) error {
	for i := 0; i < d.width; i++ {
		var v byte
		v = buf[i]
		if int(v) >= d.dstPalNumEntries {
			return FormatError("palette index out of range")
		}
		d.img_Paletted.Pix[j*d.img_Paletted.Stride+i] = v
	}
	return nil
}

func decodeRow_24(d *decoder, buf []byte, j int) error {
	for i := 0; i < d.width; i++ {
		var r, g, b byte
		b = buf[i*3+0]
		g = buf[i*3+1]
		r = buf[i*3+2]
		d.img_NRGBA.Pix[j*d.img_NRGBA.Stride+i*4+0] = r
		d.img_NRGBA.Pix[j*d.img_NRGBA.Stride+i*4+1] = g
		d.img_NRGBA.Pix[j*d.img_NRGBA.Stride+i*4+2] = b
		d.img_NRGBA.Pix[j*d.img_NRGBA.Stride+i*4+3] = 255
	}
	return nil
}

type decodeRowFuncType func(d *decoder, buf []byte, j int) error

func (d *decoder) readBits() error {
	var decodeRowFunc decodeRowFuncType
	var srcRowStride int
	var err error

	srcRowStride = ((d.width*d.bitCount + 31) / 32) * 4

	buf := make([]byte, srcRowStride)

	switch d.bitCount {
	case 1:
		decodeRowFunc = decodeRow_1
	case 4:
		decodeRowFunc = decodeRow_4
	case 8:
		decodeRowFunc = decodeRow_8
	case 24:
		decodeRowFunc = decodeRow_24
	default:
		return nil
	}

	for srcRow := 0; srcRow < d.height; srcRow++ {
		var dstRow int

		if d.isTopDown {
			dstRow = srcRow
		} else {
			dstRow = d.height - srcRow - 1
		}

		_, err = io.ReadFull(d.r, buf)
		if err != nil {
			return err
		}
		err = decodeRowFunc(d, buf, dstRow)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) skipBytes(n int) error {
	var buf [1024]byte

	for n > 0 {
		bytesToRead := len(buf)
		if bytesToRead > n {
			bytesToRead = n
		}
		_, err := io.ReadFull(d.r, buf[:bytesToRead])
		if err != nil {
			return err
		}
		n -= bytesToRead
	}
	return nil
}

// If there is a gap before the bits, skip over it.
func (d *decoder) readGap() error {
	var currentOffset int
	var gapSize int
	currentOffset = 14 + int(d.headerSize) + d.bitFieldsSize + d.srcPalSizeInBytes

	if currentOffset == int(d.bfOffBits) {
		return nil
	}
	if currentOffset > int(d.bfOffBits) {
		return FormatError("bad bfOffBits field")
	}
	gapSize = int(d.bfOffBits) - currentOffset

	return d.skipBytes(gapSize)
}

type decodeInfoHeaderFuncType func(d *decoder, h []byte, configOnly bool) error

// Read a 40-byte BITMAPINFOHEADER.
// Use of this function does not imply that the entire header is 40 bytes.
// We may just be decoding the first 40 bytes of a 108- or 124-byte header.
func decodeInfoHeader40(d *decoder, h []byte, configOnly bool) error {
	d.width = int(int32(getDWORD(h[4:8])))
	if d.width < 1 {
		return FormatError(fmt.Sprintf("bad width %d", d.width))
	}
	d.height = int(int32(getDWORD(h[8:12])))
	if d.height < 0 {
		d.isTopDown = true
		d.height = -d.height
	}
	if d.height < 1 {
		return FormatError(fmt.Sprintf("bad height %d", d.height))
	}
	d.bitCount = int(getWORD(h[14:16]))
	if configOnly {
		return nil
	}
	d.biCompression = getDWORD(h[16:20])
	if d.biCompression != bI_RGB && d.biCompression != bI_RLE4 &&
		d.biCompression != bI_RLE8 && d.biCompression != bI_BITFIELDS {
		return UnsupportedError(fmt.Sprintf("compression or image type %d", d.biCompression))
	}
	if d.biCompression == bI_BITFIELDS && d.headerSize == 40 {
		d.hasBitfieldsSegment = true
		d.bitFieldsSize = 12
	}

	biClrUsed := getDWORD(h[32:36])
	if biClrUsed > 10000 {
		return FormatError(fmt.Sprintf("bad palette size %d", biClrUsed))
	}
	d.srcPalBytesPerEntry = 4

	if d.bitCount >= 1 && d.bitCount <= 8 {
		if biClrUsed == 0 {
			d.srcPalNumEntries = 1 << uint(d.bitCount)
		} else {
			d.srcPalNumEntries = int(biClrUsed)
		}
	} else {
		d.srcPalNumEntries = int(biClrUsed)
	}

	return nil
}

func readInfoHeader(d *decoder, decodeFn decodeInfoHeaderFuncType, configOnly bool) error {
	var h []byte
	var err error

	// Read the rest of the infoheader
	h = make([]byte, d.headerSize)
	_, err = io.ReadFull(d.r, h[4:])
	if err != nil {
		return err
	}

	err = decodeFn(d, h, configOnly)
	if err != nil {
		return err
	}

	if d.bitCount >= 1 && d.bitCount <= 8 {
		d.dstHasPalette = true
	}

	return nil
}

func (d *decoder) readPalette() error {
	var err error
	buf := make([]byte, d.srcPalSizeInBytes)
	_, err = io.ReadFull(d.r, buf)
	if err != nil {
		return err
	}

	if !d.dstHasPalette {
		d.dstPalNumEntries = 0
		return nil
	}

	d.dstPalNumEntries = d.srcPalNumEntries
	if d.dstPalNumEntries > 256 {
		d.dstPalNumEntries = 256
	}

	d.dstPalette = make(color.Palette, d.dstPalNumEntries)
	for i := 0; i < d.dstPalNumEntries; i++ {
		var r, g, b byte
		b = buf[i*d.srcPalBytesPerEntry+0]
		g = buf[i*d.srcPalBytesPerEntry+1]
		r = buf[i*d.srcPalBytesPerEntry+2]
		d.dstPalette[i] = color.RGBA{r, g, b, 255}
	}
	return nil
}

func (d *decoder) decodeFileHeader(b []byte) error {
	if b[0] != 0x42 || b[1] != 0x4d {
		return FormatError("not a BMP file")
	}
	d.bfOffBits = getDWORD(b[10:14])
	return nil
}

func (d *decoder) readHeaders(configOnly bool) error {
	var fh [18]byte
	var err error

	// Read the file header, and the first 4 bytes of the info header
	_, err = io.ReadFull(d.r, fh[:])
	if err != nil {
		return err
	}

	err = d.decodeFileHeader(fh[0:14])
	if err != nil {
		return err
	}

	d.headerSize = getDWORD(fh[14:18])

	switch d.headerSize {
	case 40:
		err = readInfoHeader(d, decodeInfoHeader40, configOnly)
	default:
		return UnsupportedError(fmt.Sprintf("BMP version (header size %d)", d.headerSize))
	}
	if err != nil {
		return err
	}

	d.srcPalSizeInBytes = d.srcPalNumEntries * d.srcPalBytesPerEntry

	return nil
}

// Decodeconfig returns the color model and dimensions of the BMP image without
// decoding the entire image.
func DecodeConfig(r io.Reader) (image.Config, error) {
	var err error
	var cfg image.Config

	d := new(decoder)
	d.r = r
	err = d.readHeaders(true)
	if err != nil {
		return cfg, err
	}

	cfg.Width = d.width
	cfg.Height = d.height
	if d.dstHasPalette {
		cfg.ColorModel = color.Palette(nil)
	} else {
		cfg.ColorModel = color.NRGBAModel
	}

	return cfg, nil
}

// Decode reads a BMP image from r and returns it as an image.Image.
func Decode(r io.Reader) (image.Image, error) {
	var err error

	d := new(decoder)
	d.r = r
	err = d.readHeaders(false)
	if err != nil {
		return nil, err
	}

	if d.biCompression != bI_RGB {
		return nil, UnsupportedError(fmt.Sprintf("compression or image type %d", d.biCompression))
	}
	switch d.bitCount {
	case 1, 4, 8, 24:
	default:
		return nil, UnsupportedError(fmt.Sprintf("bit count %d", d.bitCount))
	}

	if d.srcPalNumEntries > 0 {
		err = d.readPalette()
		if err != nil {
			return nil, err
		}
	}

	// Create the target image
	if d.dstHasPalette {
		d.img_Paletted = image.NewPaletted(image.Rect(0, 0, d.width, d.height), d.dstPalette)
	} else {
		d.img_NRGBA = image.NewNRGBA(image.Rect(0, 0, d.width, d.height))
	}

	err = d.readGap()
	if err != nil {
		return nil, err
	}

	err = d.readBits()
	if err != nil {
		return nil, err
	}

	if d.dstHasPalette {
		return d.img_Paletted, nil
	}
	return d.img_NRGBA, nil
}

func init() {
	image.RegisterFormat("bmp", "BM????\x00\x00\x00\x00", Decode, DecodeConfig)
}
