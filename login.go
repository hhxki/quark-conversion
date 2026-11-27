package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/boombuler/barcode"
	"github.com/boombuler/barcode/qr"
)

const qrHost = "https://uop.quark.cn/cas/ajax/"

type qrLogin struct {
	token   string
	expires time.Time
	next    time.Time
	client  *http.Client
	png     []byte
}

func newQR(ctx context.Context) (*qrLogin, error) {
	jar, e := cookiejar.New(nil)
	if e != nil {
		return nil, e
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login := &qrLogin{client: client, expires: time.Now().Add(4 * time.Minute)}
	response, e := login.cas(ctx, "getTokenForQrcodeLogin")
	if e != nil {
		return nil, e
	}
	login.token = response.Data.Members.Token
	if response.Status != 2000000 || login.token == "" {
		return nil, errors.New("夸克未返回登录二维码，可使用会话导入")
	}
	link := "https://su.quark.cn/4_eMHBJ?" + url.Values{
		"token": {login.token}, "client_id": {"532"}, "ssb": {"weblogin"}, "uc_param_str": {""},
		"uc_biz_str": {"S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0"},
	}.Encode()
	code, e := qr.Encode(link, qr.M, qr.Auto)
	if e != nil {
		return nil, e
	}
	code, e = barcode.Scale(code, 264, 264)
	if e != nil {
		return nil, e
	}
	var out bytes.Buffer
	if e = png.Encode(&out, code); e != nil {
		return nil, e
	}
	login.png = out.Bytes()
	return login, nil
}

type casResult struct {
	Status int
	Data   struct {
		Members struct {
			Token  string
			Ticket string `json:"service_ticket"`
		}
	}
}

func (q *qrLogin) cas(ctx context.Context, action string) (casResult, error) {
	var id [16]byte
	if _, e := rand.Read(id[:]); e != nil {
		return casResult{}, e
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	raw := hex.EncodeToString(id[:])
	requestID := raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
	params := url.Values{"client_id": {"532"}, "v": {"1.2"}, "request_id": {requestID}}
	if q.token != "" {
		params.Set("token", q.token)
	}
	req, e := http.NewRequestWithContext(ctx, "GET", qrHost+action+"?"+params.Encode(), nil)
	if e != nil {
		return casResult{}, e
	}
	req.Header.Set("Accept", "application/json")
	remote, e := q.client.Do(req)
	if e != nil {
		return casResult{}, errors.New("无法连接夸克扫码服务")
	}
	defer remote.Body.Close()
	if remote.StatusCode == http.StatusTooManyRequests {
		return casResult{}, errRate
	}
	if remote.StatusCode != http.StatusOK {
		return casResult{}, errChallenge
	}
	var payload casResult
	if e = json.NewDecoder(io.LimitReader(remote.Body, 1<<20)).Decode(&payload); e != nil {
		return payload, errors.New("夸克扫码服务返回了无法识别的内容")
	}
	return payload, nil
}

func (q *qrLogin) check(ctx context.Context) (string, error) {
	if time.Now().After(q.expires) {
		return "", errors.New("二维码已过期，请刷新")
	}
	if time.Now().Before(q.next) {
		return "", nil
	}
	q.next = time.Now().Add(2 * time.Second)
	response, e := q.cas(ctx, "getServiceTicketByQrcodeToken")
	if e != nil {
		return "", e
	}
	switch response.Status {
	case 50004001:
		return "", nil
	case 2000000:
		if response.Data.Members.Ticket == "" {
			return "", nil
		}
	default:
		return "", errors.New("扫码未完成或已失效，请刷新")
	}
	endpoint := "https://pan.quark.cn/account/info?" + url.Values{"st": {response.Data.Members.Ticket}, "lw": {"scan"}}.Encode()
	req, e := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if e != nil {
		return "", e
	}
	remote, e := q.client.Do(req)
	if e != nil {
		return "", errors.New("扫码已确认，但无法交换夸克会话")
	}
	remote.Body.Close()
	if remote.StatusCode != http.StatusOK {
		return "", errors.New("扫码已确认，但夸克拒绝会话交换")
	}
	drive, _ := url.Parse("https://drive-pc.quark.cn/1/clouddrive")
	seen := map[string]bool{}
	var pairs []string
	for _, site := range []*url.URL{drive, req.URL} {
		for _, cookie := range q.client.Jar.Cookies(site) {
			if !seen[cookie.Name] {
				seen[cookie.Name] = true
				pairs = append(pairs, cookie.Name+"="+cookie.Value)
			}
		}
	}
	if len(pairs) == 0 {
		return "", errors.New("夸克未返回登录会话，可改用会话导入")
	}
	return strings.Join(pairs, "; "), nil
}
