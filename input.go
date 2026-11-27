package main

import (
	"errors"
	"math/big"
	"net/url"
	"regexp"
	"strings"
)

const defaultLimit = "100G"

type source struct {
	ID        string
	URL       string
	Code      string
	Ambiguous bool
	Attach    bool
}

func (s source) address() string {
	u, err := url.Parse(s.URL)
	if err != nil || s.Code == "" || s.Ambiguous {
		return s.URL
	}
	query := u.Query()
	if query.Get("pwd") == "" && query.Get("passcode") == "" {
		query.Set("pwd", s.Code)
		u.RawQuery = query.Encode()
	}
	return u.String()
}

var linkPattern = regexp.MustCompile("(?i)https?://(?:pan|drive)\\.quark\\.cn/s/([a-z0-9]+)(?:\\?[^\\s<>\"'，。；、）】》]+)?")
var passPattern = regexp.MustCompile("(?i)(?:提取码|访问码|密码|passcode)\\s*[:：]?\\s*([a-z0-9]{4,12})\\b")
var limitPattern = regexp.MustCompile("(?i)^([0-9]{1,12}(?:\\.[0-9]{1,9})?)\\s*(G|T)(?:I?B)?$")

func sources(text string) []source {
	var result []source
	input := strings.ReplaceAll(text, "\u00a0", " ")
	positions := linkPattern.FindAllStringSubmatchIndex(input, -1)
	seen := make(map[string]int, len(positions))
	for i, p := range positions {
		if p[1] < len(input) && strings.ContainsRune("-_@/", rune(input[p[1]])) {
			continue
		}
		raw := strings.TrimRight(input[p[0]:p[1]], ".,;)]}")
		link, err := url.Parse(raw)
		if err != nil {
			continue
		}
		item := source{ID: input[p[2]:p[3]], URL: raw, Code: link.Query().Get("pwd")}
		if item.Code == "" {
			item.Code = link.Query().Get("passcode")
		}
		if item.Code == "" {
			start := p[1]
			end := len(input)
			if i+1 < len(positions) {
				end = positions[i+1][0]
			}
			segment := input[start:end]
			if lines := strings.SplitN(segment, "\n", 3); len(lines) == 3 {
				segment = strings.Join(lines[:2], "\n")
			}
			codes := passPattern.FindAllStringSubmatch(segment, -1)
			if len(codes) == 1 {
				item.Code = codes[0][1]
			} else if len(codes) > 1 {
				item.Ambiguous = true
			}
		}
		if j, ok := seen[item.ID]; ok {
			old := &result[j]
			if old.Code == "" && item.Code != "" {
				old.Code, old.Ambiguous = item.Code, item.Ambiguous
			} else if item.Code != "" && old.Code != item.Code {
				old.Ambiguous = true
			}
			continue
		}
		seen[item.ID] = len(result)
		result = append(result, item)
	}
	return result
}

func parseLimit(value string) (int64, error) {
	m := limitPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return 0, errors.New("大小上限格式应为 100G 或 0.5T")
	}
	units := int64(1 << 30)
	if strings.EqualFold(m[2], "T") {
		units = 1 << 40
	}
	number, _ := new(big.Rat).SetString(m[1])
	number.Mul(number, big.NewRat(units, 1))
	whole := new(big.Int).Quo(number.Num(), number.Denom())
	if !whole.IsInt64() || whole.Sign() <= 0 {
		return 0, errors.New("大小上限超出可用范围")
	}
	return whole.Int64(), nil
}

func validShareURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	if u.Host != "pan.quark.cn" && u.Host != "drive.quark.cn" {
		return false
	}
	return linkPattern.FindString(value) == value
}
