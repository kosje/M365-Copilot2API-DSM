package web

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// imageStylePresets maps the chat UI style presets to prompt descriptors.
// "auto" (empty value from the UI) means no style injection — the upstream
// model decides freely.
var imageStylePresets = map[string]string{
	"":           "",
	"auto":       "",
	"photo":      "photorealistic photography, natural lighting, sharp focus, high detail",
	"anime":      "anime illustration style, clean line art, vibrant colors, cel shading",
	"3d":         "3D render, C4D, octane render, soft studio lighting, high detail",
	"flat":       "flat vector illustration, minimal geometric shapes, bold colors",
	"watercolor": "watercolor painting, soft washes, paper texture, delicate edges",
	"oil":        "classical oil painting, rich texture, visible brush strokes",
	"pixel":      "pixel art style, 16-bit retro game aesthetic, crisp pixels",
	"cyberpunk":  "cyberpunk style, neon lights, rain-slicked streets, futuristic mood",
	"ink":        "Chinese ink wash painting (shuimo), elegant brushwork, minimal composition",
	"sketch":     "minimal single-weight line art sketch, clean white background",
	"sticker":    "cute kawaii sticker design, thick white outline, rounded shapes, flat colors",
	"emote":      "chibi meme sticker, exaggerated expression, simple flat background",
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
	// Accept aspect-ratio labels (e.g. "16:9", "竖屏") and translate them to the
	// pixel size the upstream GPT Image 2 pipeline expects.
	if px, ok := ratioToImageSize[s]; ok {
		return px
	}
	return "1024x1024"
}

// systemReminderRe / htmlTagRe strip client-injected context and markup that
// some chat clients (e.g. WorkBuddy) append to the user turn. Without removal
// the block leaks into the GPT Image 2 prompt (polluting the generated image)
// and into the markdown alt text, where the leading "<" makes CommonMark
// renderers treat it as a raw HTML tag and break the image link entirely.
var systemReminderRe = regexp.MustCompile(`(?is)<system-reminder[\s\S]*?</system-reminder>`)
var htmlTagRe = regexp.MustCompile(`(?is)<[^>]+>`)

// cleanImagePrompt removes agent-context blocks and any HTML-like markup from a
// user message, then collapses all whitespace (including newlines) into single
// spaces. The result is safe to use as an image-generation prompt, a markdown
// alt text, and a displayed description.
func cleanImagePrompt(s string) string {
	s = systemReminderRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// imageGenOptions carries the full set of image-generation parameters that can
// be supplied either via the OpenAI-compatible chat request body
// (`image_options`) or parsed from the natural-language prompt text.
type imageGenOptions struct {
	Size      string `json:"size"`
	Count     int    `json:"count"`
	Style     string `json:"style"`
	Negative  string `json:"negative"`
	Quality   string `json:"quality"`
	EditImage string `json:"editImage"` // data URI of a base image for 图生图 (edit) mode
}

// derefImageOptions returns the value behind a possibly-nil pointer, defaulting
// to an empty option set. Used when merging explicitly-supplied image_options
// with natural-language hints parsed from the prompt.
func derefImageOptions(p *imageGenOptions) imageGenOptions {
	if p == nil {
		return imageGenOptions{}
	}
	return *p
}

// ratioToImageSize maps aspect-ratio labels (and a few Chinese synonyms) to the
// pixel sizes accepted by the upstream GPT Image 2 pipeline.
var ratioToImageSize = map[string]string{
	"1:1": "1024x1024", "正方形": "1024x1024", "方图": "1024x1024",
	"3:4": "864x1152", "4:3": "1152x864",
	"2:3": "832x1216", "3:2": "1216x832",
	"9:16": "720x1280", "竖屏": "720x1280", "竖图": "720x1280",
	"16:9": "1280x720", "横屏": "1280x720", "横图": "1280x720",
}

// styleKeywords maps user-spoken style synonyms (Chinese + English) to the
// canonical style keys understood by composeImagePrompt.
var styleKeywords = map[string]string{
	"写实": "photo", "写实摄影": "photo", "照片": "photo", "photo": "photo", "photorealistic": "photo",
	"动漫": "anime", "二次元": "anime", "anime": "anime",
	"3d": "3d", "3d渲染": "3d", "c4d": "3d", "三维": "3d",
	"扁平": "flat", "扁平插画": "flat", "flat": "flat",
	"水彩": "watercolor", "watercolor": "watercolor",
	"油画": "oil", "oil": "oil",
	"像素": "pixel", "像素艺术": "pixel", "pixel": "pixel", "像素风": "pixel",
	"赛博朋克": "cyberpunk", "cyberpunk": "cyberpunk",
	"水墨": "ink", "ink": "ink", "国风": "ink",
	"线条": "sketch", "线稿": "sketch", "sketch": "sketch",
	"贴纸": "sticker", "sticker": "sticker", "卡通贴纸": "sticker",
	"表情包": "emote", "emote": "emote", "chibi": "emote",
}

// normalize returns the validated size + clamped count for this option set.
func (o imageGenOptions) normalize() (size string, count int) {
	size = normalizeImageSize(o.Size)
	count = o.Count
	if count <= 0 {
		count = 1
	}
	if count > 4 {
		count = 4
	}
	return
}

// buildPrompt composes the final upstream GPT Image 2 prompt from the raw
// description and the style/negative/quality options, prefixing the size.
func (o imageGenOptions) buildPrompt(description string) string {
	composed := composeImagePrompt(description, o.Style, o.Negative, o.Quality)
	size := normalizeImageSize(o.Size)
	return fmt.Sprintf("Generate an image with GPT Image 2. Size: %s. %s. Return the image URL directly.", size, composed)
}

// parseImageOptionsFromText fills in any option fields left empty by scanning
// the user's natural-language prompt for size / style / count / quality /
// negative hints. Structured `base` values (e.g. from image_options) win; only
// blank fields are inferred from text. This lets API clients either pass explicit
// JSON parameters or simply describe what they want in Chinese/English.
func parseImageOptionsFromText(text string, base imageGenOptions) imageGenOptions {
	out := base
	low := strings.ToLower(text)

	// size: explicit "1024x1024" or ratio label / Chinese synonym
	if out.Size == "" {
		for ratio, px := range ratioToImageSize {
			if strings.Contains(text, ratio) {
				out.Size = px
				break
			}
		}
		if out.Size == "" {
			// direct WxH token, but only if it is a valid size
			for vs := range validImageSizes {
				if strings.Contains(low, vs) {
					out.Size = vs
					break
				}
			}
		}
	}

	// style
	if out.Style == "" {
		for kw, key := range styleKeywords {
			if strings.Contains(text, kw) {
				out.Style = key
				break
			}
		}
	}

	// count: "<number>张/幅/张图/张图片" (Chinese), or "N images"
	if out.Count <= 0 {
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`(\d{1,2})\s*(张|幅|张图|张图片|张图)`),
			regexp.MustCompile(`(\d{1,2})\s*images?`),
		} {
			if m := re.FindStringSubmatch(text); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
					out.Count = n
					break
				}
			}
		}
	}

	// quality: 高清 / hd
	if out.Quality == "" {
		if strings.Contains(text, "高清") || strings.Contains(low, "hd") || strings.Contains(text, "超清") {
			out.Quality = "hd"
		}
	}

	// negative: clause after explicit negative keywords
	if out.Negative == "" {
		for _, kw := range []string{"不要有", "不要包含", "不要", "避免", "去除", "去掉", "请勿", "别加", "别出现", "不要出现"} {
			if idx := strings.Index(text, kw); idx >= 0 {
				tail := text[idx+len(kw):]
				// trim at sentence punctuation
				for _, p := range []string{"。", "，", ",", "！", "!", "？", "?", "；", ";", "\n"} {
					if cut := strings.Index(tail, p); cut >= 0 {
						tail = tail[:cut]
					}
				}
				tail = strings.TrimSpace(tail)
				if tail != "" && len([]rune(tail)) <= 40 {
					out.Negative = tail
				}
				break
			}
		}
	}

	return out
}

// imageGenOptionsSummary renders a human-readable summary of the resolved
// options for the "完整提示词建议" block returned to the caller.
func (o imageGenOptions) summary(description string) string {
	size, count := o.normalize()
	var b strings.Builder
	ratio := imageRatioLabels[size]
	if ratio == "" {
		ratio = size
	}
	b.WriteString("【本次生图参数】\n")
	fmt.Fprintf(&b, "• 描述：%s\n", strings.TrimSpace(description))
	fmt.Fprintf(&b, "• 比例：%s（%s）\n", ratio, size)
	if l, ok := imageStyleLabels[strings.ToLower(o.Style)]; ok {
		fmt.Fprintf(&b, "• 风格：%s\n", l)
	} else if o.Style != "" {
		fmt.Fprintf(&b, "• 风格：%s\n", o.Style)
	}
	if strings.EqualFold(o.Quality, "hd") {
		b.WriteString("• 质量：高清\n")
	}
	if count > 1 {
		fmt.Fprintf(&b, "• 数量：%d 张\n", count)
	}
	if o.Negative != "" {
		fmt.Fprintf(&b, "• 排除：%s\n", o.Negative)
	}
	// full re-usable prompt built the same way the upstream receives it
	fmt.Fprintf(&b, "\n完整提示词（可直接复用）：\n%s\n", o.buildPrompt(description))
	return b.String()
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
