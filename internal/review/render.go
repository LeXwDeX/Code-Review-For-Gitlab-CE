package review

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const SummaryMarkerPrefix = "<!-- gitlab-ce-review:summary"
const FindingMarkerPrefix = "<!-- gitlab-ce-review:finding"

var markerFingerprintRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var publishKeyRE = regexp.MustCompile(`^pk_[0-9a-f]{32}$`)

// SummaryInput is structured data for the summary note.
type SummaryInput struct {
	Verdict              string
	Summary              string
	Groups               []Group
	Fingerprint          string
	HeadSHA              string
	Profile              string
	Duration             time.Duration
	DetailURL            string
	ShowModelAttribution bool
	Warnings             []string
	IncompleteReason     string
}

// FindingRenderOptions controls optional attribution in an inline discussion.
type FindingRenderOptions struct {
	ShowModelAttribution bool
}

// SummaryMarkerLine returns the hidden first line of the summary note.
func SummaryMarkerLine(fingerprint string) (string, error) {
	if !markerFingerprintRE.MatchString(fingerprint) {
		return "", invalid("fingerprint", "must be a safe token of at most 128 characters")
	}
	return fmt.Sprintf("%s v1 fp=%s -->", SummaryMarkerPrefix, fingerprint), nil
}

// RenderSummary renders a summary note using only structured fields. All model
// text is escaped; the first line is always the official hidden marker.
func RenderSummary(in SummaryInput) (string, error) {
	switch in.Verdict {
	case VerdictPass, VerdictBlocking, VerdictIncomplete:
	default:
		return "", invalid("verdict", "must be PASS, BLOCKING, or INCOMPLETE")
	}
	marker, err := SummaryMarkerLine(in.Fingerprint)
	if err != nil {
		return "", err
	}
	summary := firstParagraph(in.Summary)
	if in.Verdict != VerdictIncomplete && strings.TrimSpace(summary) == "" {
		return "", invalid("summary", "must not be empty")
	}

	counts := map[Severity]int{SeverityP0: 0, SeverityP1: 0, SeverityP2: 0, SeverityInfo: 0}
	marked := make([]Group, 0)
	unpositioned := make([]Group, 0)
	for _, group := range in.Groups {
		if !group.Published() {
			continue
		}
		severity := group.EffectiveSeverity()
		if _, ok := counts[severity]; ok {
			counts[severity]++
		}
		if group.Decision.Action == ActionMark {
			marked = append(marked, group)
			continue
		}
		if !group.PositionOK {
			unpositioned = append(unpositioned, group)
		}
	}

	var b strings.Builder
	b.WriteString(marker)
	b.WriteByte('\n')
	b.WriteString("### AI 评审：")
	b.WriteString(verdictLabel(in.Verdict))
	b.WriteString("\n\n")
	if summary != "" {
		b.WriteString("**结论**：")
		b.WriteString(escapeText(summary, MaxSummaryRunes))
		b.WriteString("\n\n")
	}
	b.WriteString("| 级别 | 数量 |\n|---|---|\n")
	for _, severity := range []Severity{SeverityP0, SeverityP1, SeverityP2, SeverityInfo} {
		fmt.Fprintf(&b, "| %s | %d |\n", severity, counts[severity])
	}
	if len(unpositioned) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>无法定位到行的问题（%d）</summary>\n\n", len(unpositioned))
		for _, group := range unpositioned {
			b.WriteString(renderSummaryItem(group, false))
		}
		b.WriteString("\n</details>\n")
	}
	if len(marked) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>存疑条目（%d）</summary>\n\n", len(marked))
		for _, group := range marked {
			b.WriteString(renderSummaryItem(group, true))
		}
		b.WriteString("\n</details>\n")
	}
	if len(in.Warnings) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>评审提示（%d）</summary>\n\n", len(in.Warnings))
		for _, warning := range in.Warnings {
			b.WriteString("- ")
			b.WriteString(escapeOneLine(warning, 500))
			b.WriteByte('\n')
		}
		b.WriteString("\n</details>\n")
	}
	if in.Verdict == VerdictIncomplete && strings.TrimSpace(in.IncompleteReason) != "" {
		b.WriteString("\n> 评审未完成：")
		b.WriteString(escapeOneLine(in.IncompleteReason, 500))
		b.WriteByte('\n')
	}

	footer := make([]string, 0, 4)
	if in.HeadSHA != "" && markerFingerprintRE.MatchString(in.HeadSHA) {
		footer = append(footer, "head "+in.HeadSHA)
	}
	if in.Profile != "" && markerFingerprintRE.MatchString(in.Profile) {
		footer = append(footer, "profile "+in.Profile)
	}
	if in.Duration > 0 {
		footer = append(footer, "用时 "+in.Duration.Round(time.Second).String())
	}
	if link := safeDetailURL(in.DetailURL); link != "" {
		footer = append(footer, fmt.Sprintf(`<a href="%s">详情</a>`, link))
	}
	if len(footer) > 0 {
		b.WriteString("\n<sub>")
		b.WriteString(strings.Join(footer, " · "))
		b.WriteString("</sub>\n")
	}
	return b.String(), nil
}

func verdictLabel(verdict string) string {
	switch verdict {
	case VerdictPass:
		return "🟢 PASS"
	case VerdictBlocking:
		return "🔴 BLOCKING"
	default:
		return "⚪ 评审未完成"
	}
}

func renderSummaryItem(group Group, marked bool) string {
	severity := group.EffectiveSeverity()
	display := string(severity)
	if marked {
		display += "?"
	}
	line := "- **[" + display + "] " + escapeOneLine(group.Title, MaxTitleRunes) + "**"
	if group.Path != "" {
		line += " — " + inlineCode(group.Path)
	} else {
		line += " — 无代码位置"
	}
	if marked {
		reason := "挑战结果：UNCERTAIN"
		if group.Challenge != nil && group.Challenge.Reason != "" {
			reason = "挑战结果：" + oneLine(group.Challenge.Reason, 300)
		}
		if group.Decision != nil && group.Decision.Reason != "" {
			reason += "；" + oneLine(group.Decision.Reason, 300)
		}
		line += " — " + escapeOneLine(reason, 600)
	} else {
		line += " — " + escapeOneLine(firstParagraph(group.Body), 300)
	}
	return line + "\n"
}

// RenderFinding renders one inline discussion. The first line is a stable
// hidden key derived from path, normalized title, and snippet.
func RenderFinding(group Group, opts FindingRenderOptions) (string, error) {
	if !group.Published() {
		return "", invalid("decision", "only publish or mark groups can be rendered inline")
	}
	if !group.PositionOK {
		return "", invalid("position_ok", "finding has no diff hunk and must be published in the summary only")
	}
	key := group.PublishKey
	if key == "" {
		key = PublishKeyFor(group.Path, group.Title, group.Evidence)
	}
	if !publishKeyRE.MatchString(key) {
		return "", invalid("publish_key", "must be a stable pk_ key")
	}
	severity := group.EffectiveSeverity()
	display := string(severity)
	if group.Decision.Action == ActionMark {
		display += "?"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s key=%s -->\n", FindingMarkerPrefix, key)
	if group.Decision.Action == ActionMark {
		b.WriteString("**[存疑] ")
	} else {
		b.WriteString("**")
	}
	b.WriteString("[")
	b.WriteString(display)
	b.WriteString("] ")
	b.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
	b.WriteString("**\n")
	if group.Path != "" {
		b.WriteString("\n位置：")
		b.WriteString(inlineCode(group.Path))
		if group.Line > 0 {
			fmt.Fprintf(&b, ":%d", group.Line)
		}
		b.WriteByte('\n')
	}
	if body := escapeText(group.Body, MaxBodyRunes); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteByte('\n')
	}
	if strings.TrimSpace(group.Evidence) != "" {
		b.WriteString("\n<details><summary>证据</summary>\n\n")
		b.WriteString(codeFence(group.Evidence))
		b.WriteString("\n\n</details>\n")
	}
	if suggestion := escapeText(group.Suggestion, MaxSuggestionRunes); suggestion != "" {
		b.WriteString("\n**建议**：")
		b.WriteString(suggestion)
		b.WriteByte('\n')
	}
	if group.Decision.Action == ActionMark {
		b.WriteString("\n**标记**：")
		b.WriteString(escapeOneLine(group.Decision.Reason, 500))
		b.WriteByte('\n')
	}
	if opts.ShowModelAttribution {
		if attribution := modelAttribution(group); attribution != "" {
			b.WriteString("\n<sub>来源：")
			b.WriteString(attribution)
			b.WriteString("</sub>\n")
		}
	}
	return b.String(), nil
}

func modelAttribution(group Group) string {
	seen := make(map[string]struct{})
	values := make([]string, 0, 3)
	for _, source := range group.Sources {
		name := strings.TrimSpace(source.Origin.Provider)
		if source.Origin.Model != "" {
			if name != "" {
				name += "/" + source.Origin.Model
			} else {
				name = source.Origin.Model
			}
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		values = append(values, escapeOneLine(name, 128))
	}
	return strings.Join(values, "、")
}

func firstParagraph(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(value)
	if i := strings.Index(value, "\n\n"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateRunes(value, max)
}

func escapeOneLine(value string, max int) string {
	return escapeText(oneLine(value, max), max)
}

func escapeText(value string, max int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = truncateRunes(value, max)
	value = html.EscapeString(value)
	replacer := strings.NewReplacer(
		"[", "&#91;",
		"]", "&#93;",
		"`", "&#96;",
	)
	return replacer.Replace(value)
}

func inlineCode(value string) string {
	value = oneLine(value, MaxPathBytes)
	if !strings.Contains(value, "`") {
		return "`" + value + "`"
	}
	fence := "``"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	return fence + " " + value + " " + fence
}

func codeFence(value string) string {
	value = truncateRunes(value, MaxEvidenceRunes)
	value = strings.ReplaceAll(value, "<!--", "&lt;!--")
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	return fence + "\n" + value + "\n" + fence
}

func safeDetailURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return html.EscapeString(raw)
}
