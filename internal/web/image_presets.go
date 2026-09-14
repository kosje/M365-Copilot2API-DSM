package web

import (
	"fmt"
	"strings"
)

// imageStylePresets maps the chat UI style presets to prompt descriptors.
// "auto" (empty value from the UI) means no style injection — the upstream
// model decides freely.
var imageStylePresets = map[string]string{
	"":          "",
	"auto":      "",
	"photo":     "photorealistic photography, natural lighting, sharp focus, high detail",
	"anime":     "anime illustration style, clean line art, vibrant colors, cel shading",
	"3d":        "3D render, C4D, octane render, soft studio lighting, high detail",
	"flat":      "flat vector illustration, minimal geometric shapes, bold colors",
	"watercolor": "watercolor painting, soft washes, paper texture, delicate edges",
	"oil":       "classical oil painting, rich texture, visible brush strokes",
	"pixel":     "pixel art style, 16-bit retro game aesthetic, crisp pixels",
	"cyberpunk": "cyberpunk style, neon lights, rain-slicked streets, futuristic mood",
	"ink":       "Chinese ink wash painting (shuimo), elegant brushwork, minimal composition",
	"sketch":    "minimal single-weight line art sketch, clean white background",
	"sticker":   "cute kawaii sticker design, thick white outline, rounded shapes, flat colors",
	"emote":     "chibi meme sticker, exaggerated expression, simple flat background",
}

// composeImagePrompt builds the final upstream prompt from the raw description
// and the chat image panel options. Everything is prompt-injected because the
// M365 upstream is conversational, not a parameterised image API.
func composeImagePrompt(description, style, negative, quality string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(description))
	if d := imageStylePresets[strings.ToLower(strings.TrimSpace(style))]; d != "" {
		b.WriteString(". Style: ")
		b.WriteString(d)
	}
	if strings.EqualFold(strings.TrimSpace(quality), "hd") {
		b.WriteString(". Quality: ultra high definition, extremely detailed, best quality")
	}
	if neg := strings.TrimSpace(negative); neg != "" {
		b.WriteString(fmt.Sprintf(". Strictly avoid the following elements (do not include them): %s", neg))
	}
	return b.String()
}

var validImageSizes = map[string]bool{
	"1024x1024": true, // 1:1
	"864x1152":  true, // 3:4
	"1152x864":  true, // 4:3
	"832x1216":  true, // 2:3
	"1216x832":  true, // 3:2
	"720x1280":  true, // 9:16
	"1280x720":  true, // 16:9
}

func normalizeImageSize(size string) string {
	s := strings.ToLower(strings.TrimSpace(size))
	if validImageSizes[s] {
		return s
	}
	return "1024x1024"
}

// imageStyleLabels maps style keys to their Chinese display names (used in the
// persisted user message so both owner and admin can see what was requested).
var imageStyleLabels = map[string]string{
	"photo": "写实摄影", "anime": "动漫", "3d": "3D渲染", "flat": "扁平插画",
	"watercolor": "水彩", "oil": "油画", "pixel": "像素艺术", "cyberpunk": "赛博朋克",
	"ink": "水墨", "sketch": "线条画", "sticker": "贴纸", "emote": "表情包",
}

// imageRatioLabels maps pixel sizes to their aspect-ratio chip label.
var imageRatioLabels = map[string]string{
	"864x1152": "3:4", "1152x864": "4:3", "832x1216": "2:3",
	"1216x832": "3:2", "720x1280": "9:16", "1280x720": "16:9",
}

// imageUserDisplay renders the persisted user message with a short option tag.
func imageUserDisplay(prompt, size, style, quality string, count int, isEdit bool) string {
	var tags []string
	if isEdit {
		tags = append(tags, "图生图")
	}
	if l := imageRatioLabels[strings.TrimSpace(size)]; l != "" {
		tags = append(tags, l)
	} else if size != "" && size != "1024x1024" {
		tags = append(tags, size)
	}
	if l := imageStyleLabels[strings.ToLower(strings.TrimSpace(style))]; l != "" {
		tags = append(tags, l)
	}
	if strings.EqualFold(strings.TrimSpace(quality), "hd") {
		tags = append(tags, "高清")
	}
	if count > 1 {
		tags = append(tags, fmt.Sprintf("x%d", count))
	}
	if len(tags) == 0 {
		return prompt
	}
	return prompt + "（" + strings.Join(tags, " · ") + "）"
}
