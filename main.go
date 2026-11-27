package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const maxInput = 2 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, e := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
	}
	os.Exit(code)
}

func readText(r io.Reader) (string, error) {
	raw, e := io.ReadAll(io.LimitReader(r, maxInput+1))
	if e != nil {
		return "", e
	}
	if len(raw) > maxInput {
		return "", errors.New("输入不能超过 2 MiB")
	}
	return string(raw), nil
}

func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer) (int, error) {
	usage := func() {
		fmt.Fprintln(out, "quark-convert login | convert | serve | help")
		fmt.Fprintln(out, "  login --cookie-file <path>   导入已有的夸克网页会话；省略参数时从标准输入读取")
		fmt.Fprintln(out, "  convert --file <path>        从 TXT 转换；也可传入分享文案或从标准输入读取")
		fmt.Fprintln(out, "  convert --json --limit 0.5T  返回逐条 JSON 结果，使用指定大小上限")
		fmt.Fprintln(out, "  convert --attach --json     为本次转换的所有链接添加常用附件")
		fmt.Fprintln(out, "  convert --template <文本>   按 {{.Title}}、{{.URL}} 等变量输出，也可用 --template-file <路径>")
		fmt.Fprintln(out, "  serve --port 8765           打开本地网页与 JSON API")
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		usage()
		return 0, nil
	}
	if args[0] != "login" && args[0] != "convert" && args[0] != "serve" {
		return 1, errors.New("未知命令，请运行 quark-convert help")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	dd := flags.String("data-dir", "", "数据目录")
	var inputFile, limit, port *string
	var jsonMode, fresh, attachAll *bool
	var templateText, templateFile, quarkCLI string
	var shareTemplate *template.Template
	switch args[0] {
	case "login":
		inputFile = flags.String("cookie-file", "", "从指定文件导入 Cookie")
	case "convert":
		inputFile = flags.String("file", "", "读取 TXT")
		limit = flags.String("limit", "", "转存大小上限")
		jsonMode = flags.Bool("json", false, "输出 JSON")
		fresh = flags.Bool("ignore-name-matches", false, "忽略需要手动确认的同名旧分享")
		attachAll = flags.Bool("attach", false, "为本次转换的所有链接添加网盘「常用附件」中的文件")
		flags.StringVar(&templateText, "template", "", "分享文案模板")
		flags.StringVar(&templateFile, "template-file", "", "读取 UTF-8 分享文案模板文件")
	case "serve":
		port = flags.String("port", "8765", "监听端口，0 为自动分配")
		flags.StringVar(&quarkCLI, "quark-cli", "", "夸克官方 quark-drive.cjs 的路径，用于手动更新")
	}
	if e := flags.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0, nil
		}
		return 1, e
	}
	if args[0] == "convert" {
		var err error
		shareTemplate, err = loadShareTemplate(templateText, templateFile, *jsonMode)
		if err != nil {
			return 1, err
		}
	}
	if *dd == "" {
		userDir, e := os.UserConfigDir()
		if e != nil {
			return 1, e
		}
		*dd = filepath.Join(userDir, "quark-convert")
	}
	s, e := openState(*dd)
	if e != nil {
		return 1, e
	}
	defer s.close()
	switch args[0] {
	case "login":
		if flags.NArg() > 0 {
			return 1, errors.New("请从文件或标准输入导入会话")
		}
		if *inputFile == "" {
			fmt.Fprintln(stderr, "请在标准输入粘贴夸克 Cookie 后结束输入，或使用网页导入。")
		}
		cookie, e := readInput(*inputFile, in)
		if e != nil {
			return 1, e
		}
		account, e := s.login(ctx, cookie)
		if e != nil {
			return 1, e
		}
		fmt.Fprintln(out, "已保存夸克会话", account.Nickname)
		s.printShareSync(ctx, out, stderr)
	case "convert":
		var input string
		if flags.NArg() > 0 && *inputFile != "" {
			return 1, errors.New("请通过文件或参数提供文本")
		}
		if flags.NArg() > 0 {
			input = strings.Join(flags.Args(), " ")
		} else {
			input, e = readInput(*inputFile, in)
			if e != nil {
				return 1, e
			}
		}
		rows, e := s.convert(ctx, input, *limit, *fresh, false, *attachAll, nil)
		if e != nil {
			return 1, e
		}
		if *jsonMode {
			if e = json.NewEncoder(out).Encode(rows); e != nil {
				return 1, e
			}
		}
		exit := 0
		for _, r := range rows {
			if r.Link != "" {
				if !*jsonMode {
					if e := writeShareText(out, shareTemplate, r); e != nil {
						return 1, e
					}
				}
			} else {
				exit = 2
				if !*jsonMode {
					fmt.Fprintf(stderr, "%s\t%s\t%s\n", r.Status, r.Source, r.Message)
				}
			}
		}
		return exit, nil
	case "serve":
		if flags.NArg() > 0 {
			return 1, errors.New("serve 只接受选项")
		}
		portNumber, e := strconv.ParseUint(*port, 10, 16)
		if e != nil {
			return 1, errors.New("--port 必须是 0 到 65535 的整数，0 为自动分配")
		}
		updates, e := openOfficialUpdates(quarkCLI)
		if e != nil {
			return 1, e
		}
		listener, e := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", portNumber))
		if e != nil {
			return 1, fmt.Errorf("无法监听端口 %d。若端口已被占用，请用 serve --port 8766 改用空闲端口，或用 --port 0 自动分配：%w", portNumber, e)
		}
		defer listener.Close()
		s.backfillAvatar(ctx)
		s.printShareSync(ctx, out, stderr)
		if ctx.Err() != nil {
			return 0, nil
		}
		server := &http.Server{Handler: web(s, listener.Addr().String(), updates), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				server.Close()
			}
		}()
		_, listenPort, _ := net.SplitHostPort(listener.Addr().String())
		fmt.Fprintf(out, "监听 %s，本机管理页 http://127.0.0.1:%s\n", listener.Addr(), listenPort)
		if e = server.Serve(listener); !errors.Is(e, http.ErrServerClosed) {
			return 1, e
		}
	}
	return 0, nil
}

func (s *state) backfillAvatar(ctx context.Context) {
	var saved session
	if s.setting("session", &saved) != nil || saved.Cookie == "" || saved.AvatarURL != "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	profile, err := newQuark(saved.Cookie).verify(ctx)
	if err != nil || profile.Account != saved.Account || profile.AvatarURL == "" {
		return
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return
	}
	defer release()
	var current session
	if s.setting("session", &current) == nil && current == saved {
		current.AvatarURL = profile.AvatarURL
		_ = s.set("session", current)
	}
}

type shareTemplateData struct {
	Title  string
	URL    string
	Source string
	Status string
}

func loadShareTemplate(text, path string, jsonMode bool) (*template.Template, error) {
	if text == "" && path == "" {
		return nil, nil
	}
	if jsonMode || (text != "" && path != "") {
		return nil, errors.New("--json、--template 和 --template-file 只能选择一种")
	}
	if path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("打开分享模板：%w", err)
		}
		defer file.Close()
		text, err = readText(file)
		if err != nil {
			return nil, fmt.Errorf("读取分享模板：%w", err)
		}
	}
	text = strings.TrimPrefix(text, "\ufeff")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("分享文案模板为空")
	}
	tpl, err := template.New("share").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("解析分享模板：%w", err)
	}
	sample := shareTemplateData{Title: "示例", URL: "https://pan.quark.cn/s/example", Source: "https://pan.quark.cn/s/source", Status: created}
	for _, status := range []string{created, reused} {
		sample.Status = status
		if err := tpl.Execute(io.Discard, sample); err != nil {
			return nil, fmt.Errorf("检查分享模板：%w", err)
		}
	}
	return tpl, nil
}

func writeShareText(out io.Writer, tpl *template.Template, row result) error {
	if tpl == nil {
		_, err := fmt.Fprintln(out, row.Link)
		return err
	}
	var text strings.Builder
	if err := tpl.Execute(&text, shareTemplateData{Title: row.Title, URL: row.Link, Source: row.Source, Status: row.Status}); err != nil {
		return fmt.Errorf("生成分享文案：%w", err)
	}
	if !strings.HasSuffix(text.String(), "\n") {
		text.WriteByte('\n')
	}
	_, err := io.WriteString(out, text.String())
	return err
}

func (s *state) printShareSync(ctx context.Context, out, stderr io.Writer) {
	account, _, err := s.config()
	if err == nil && (account.Account == "" || account.Cookie == "") {
		return
	}
	var count int
	if err == nil {
		fmt.Fprintln(out, "正在同步我的分享…")
		count, err = s.syncShares(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, "同步我的分享失败，已保存的索引可继续查询。", err)
		return
	}
	fmt.Fprintf(out, "已同步 %d 条分享\n", count)
}

func (s *state) syncShares(ctx context.Context) (int, error) {
	b, release, err := s.startBatch(ctx, "")
	if err != nil {
		return 0, err
	}
	defer release()
	shares, err := b.refreshShares(ctx)
	return len(shares), err
}

func readInput(path string, in io.Reader) (string, error) {
	if path == "" || path == "-" {
		return readText(in)
	}
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	return readText(f)
}

func (s *state) login(ctx context.Context, value string) (session, error) {
	cookie := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "\ufeff"))
	if len(cookie) > 16<<10 || strings.ContainsAny(cookie, "\x00\r\n") {
		return session{}, errors.New("请粘贴单行 Cookie 值")
	}
	if len(cookie) >= 7 && strings.EqualFold(cookie[:7], "cookie:") {
		cookie = strings.TrimSpace(cookie[7:])
	}
	if !strings.Contains(cookie, "=") {
		return session{}, errors.New("请粘贴夸克网页 Cookie 值")
	}
	release, e := s.acquire(ctx)
	if e != nil {
		return session{}, e
	}
	defer release()
	account, e := newQuark(cookie).verify(ctx)
	if e != nil {
		return session{}, e
	}
	if e = s.set("session", account); e != nil {
		return session{}, e
	}
	return account, nil
}

func (s *state) config() (session, string, error) {
	var account session
	limit := defaultLimit
	if e := s.setting("session", &account); e != nil {
		return account, limit, e
	}
	return account, limit, s.setting("limit", &limit)
}

func (s *state) startBatch(ctx context.Context, override string) (*batch, func(), error) {
	release, e := s.acquire(ctx)
	if e != nil {
		return nil, nil, e
	}
	account, limit, e := s.config()
	if e != nil {
		release()
		return nil, nil, e
	}
	if account.Account == "" || account.Cookie == "" {
		release()
		return nil, nil, errors.New("请先登录并导入夸克会话")
	}
	if override != "" {
		limit = override
	}
	size, e := parseLimit(limit)
	if e != nil {
		release()
		return nil, nil, e
	}
	q := newQuark(account.Cookie)
	key := "wait:" + account.Account
	var until time.Time
	if e = s.setting(key, &until); e != nil {
		release()
		return nil, nil, e
	}
	if delay := time.Until(until); delay > 0 {
		release()
		return nil, nil, fmt.Errorf("夸克处于限流等待中，请在约 %d 秒后重试", int64(delay/time.Second)+1)
	}
	return &batch{s: s, q: q, account: account.Account, limit: size}, func() {
		if !q.cooldown.IsZero() {
			_ = s.set(key, q.cooldown)
		}
		release()
	}, nil
}

func (s *state) convert(ctx context.Context, text, limit string, fresh, retest, attach bool, progress func(result)) ([]result, error) {
	input := sources(text)
	if len(input) == 0 {
		return nil, errors.New("没有识别到夸克分享链接")
	}
	for i := range input {
		input[i].Attach = attach
	}
	b, release, e := s.startBatch(ctx, limit)
	if e != nil {
		return nil, e
	}
	defer release()
	b.retest = retest
	return b.convert(ctx, input, fresh, progress), nil
}
