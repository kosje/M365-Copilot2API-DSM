package web

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"image/gif"
	"image/jpeg"
	"image/png"
)

// stripImageMetadata removes machine-origin provenance metadata (C2PA content
// credentials, EXIF, XMP, ICC, etc.) from a generated image so that downstream
// clients — notably iOS long-press "AI-generated" labelling — no longer expose
// the machine-origin watermark. The bytes returned carry only the pixel data:
// decoding and re-encoding with the standard library discards every auxiliary
// chunk/marker (PNG c2pa/iTXt, JPEG APP1/APP11, GIF extensions) while keeping
// the visible image. PNG re-encode is lossless; JPEG uses a high quality factor.
//
// It is intended only for AI-generated images, NOT user uploads (which may carry
// legitimate EXIF such as orientation). On any decode/encode failure the
// original bytes are returned unchanged.
func stripImageMetadata(data []byte, contentType string) ([]byte, string) {
	if len(data) == 0 {
		return data, contentType
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil || img == nil || img.Bounds().Dx() == 0 {
		return data, contentType
	}
	var buf bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			return data, contentType
		}
		return buf.Bytes(), "image/png"
	case "jpeg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return data, contentType
		}
		return buf.Bytes(), "image/jpeg"
	case "gif":
		if err := gif.Encode(&buf, img, nil); err != nil {
			return data, contentType
		}
		return buf.Bytes(), "image/gif"
	default:
		// Unsupported format (e.g. webp without x/image): keep original.
		return data, contentType
	}
}
