package scdl

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/Eyevinn/mp4ff/mp4"
)

// writeM4A joins the fragmented MP4 init and media segments, converts them
// into a progressive MP4 so players see the full duration and can seek, and
// tags the result with iTunes-style metadata.
func (c *Client) writeM4A(ctx context.Context, outPath string, initSegment []byte, segments [][]byte, track *Track) error {
	fragmented := bytes.Join(append([][]byte{initSegment}, segments...), nil)

	f, err := mp4.DecodeFile(bytes.NewReader(fragmented))
	if err != nil {
		return fmt.Errorf("decode MP4: %w", err)
	}
	if f.Moov == nil {
		return fmt.Errorf("decode MP4: no moov box in init segment")
	}

	setM4AMetadata(f.Moov, c.m4aMetadata(ctx, track))

	var out bytes.Buffer
	if err := mp4.Defragment(f, bytes.NewReader(fragmented), &out); err != nil {
		return fmt.Errorf("defragment MP4: %w", err)
	}

	if err := os.WriteFile(outPath, out.Bytes(), 0o644); err != nil { //nolint:gosec // user controls output path
		return fmt.Errorf("write output file: %w", err)
	}

	return nil
}

// m4aMetadata builds the iTunes-style metadata (udta/meta/ilst) for track.
// MP4 has no artist picture type, so only the front cover is embedded.
func (c *Client) m4aMetadata(ctx context.Context, track *Track) *mp4.UdtaBox {
	ilst := &mp4.IlstBox{}

	for _, item := range []struct{ typ, value string }{
		{"\xa9nam", track.Title},
		{"\xa9ART", track.Artist},
		{"\xa9alb", track.Album},
		{"\xa9day", track.Year},
		{"\xa9gen", track.Genre},
		{"\xa9cmt", track.Description},
	} {
		if item.value != "" {
			ilst.AddChild(m4aItem(item.typ, mp4.DataTypeUTF8, []byte(item.value)))
		}
	}

	if image, mimeType := c.fetchImage(ctx, track.ArtworkURL); image != nil {
		dataType := uint32(mp4.DataTypeJPEG)
		if mimeType == mimePNG {
			dataType = mp4.DataTypePNG
		}
		ilst.AddChild(m4aItem("covr", dataType, image))
	}

	hdlr := &mp4.HdlrBox{HandlerType: "mdir", Reserved: [3]uint32{0x6170706c}} // manufacturer "appl"
	meta := mp4.CreateMetaBox(0, hdlr)
	meta.AddChild(ilst)

	udta := &mp4.UdtaBox{}
	udta.AddChild(meta)
	return udta
}

func m4aItem(typ string, dataType uint32, data []byte) mp4.Box {
	item := mp4.NewGenericContainerBox(typ)
	item.AddChild(&mp4.DataBox{DataType: dataType, Data: data})
	return item
}

// setM4AMetadata replaces any udta in moov (SoundCloud's init segment carries
// its own encoder tags) with udta.
func setM4AMetadata(moov *mp4.MoovBox, udta *mp4.UdtaBox) {
	children := make([]mp4.Box, 0, len(moov.Children)+1)
	for _, child := range moov.Children {
		if child.Type() != "udta" {
			children = append(children, child)
		}
	}
	children = append(children, udta)
	moov.Children = children
}
