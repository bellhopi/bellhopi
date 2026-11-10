package main

import (
	"bytes"
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
	"regexp"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bellhopi/bellhopi/internal/dialer"
	"github.com/enetx/g"
)

var callIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,80}$`)

func main() {
	args := g.Slice[g.String]{}
	g.Slice[string](os.Args[1:]).Iter().ForEach(func(arg string) { args.Push(g.String(arg)) })
	result := run(args, os.Stdout)
	if result.IsErr() {
		_ = json.NewEncoder(os.Stderr).Encode(g.Map[g.String, any]{"error": result.Err().Error()})
		os.Exit(1)
	}
	os.Exit(result.Unwrap().Std())
}

func run(args g.Slice[g.String], out io.Writer) g.Result[g.Int] {
	global := flags("concierge-dialer")
	config := global.String("config", "", "private configuration path; place before command")
	if err := global.Parse(stdArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return help(out)
		}
		return g.Err[g.Int](errors.New("invalid global flags; place --config before the command"))
	}
	remaining := global.Args()
	if len(remaining) == 0 || remaining[0] == "help" {
		return help(out)
	}
	command := g.String(remaining[0])
	commandArgs := g.Slice[g.String]{}
	g.Slice[string](remaining[1:]).Iter().ForEach(func(arg string) { commandArgs.Push(g.String(arg)) })
	if command == "install" {
		return installCommand(commandArgs, out)
	}
	if !g.SliceOf[g.String]("init", "doctor", "serve", "call", "status", "reply", "dtmf", "hangup", "reconcile").Contains(command) {
		return g.Err[g.Int](errors.New("unknown command; use help"))
	}
	path := g.String(*config)
	if path.IsEmpty() {
		defaultPath := dialer.DefaultConfigPath()
		if defaultPath.IsErr() {
			return g.Err[g.Int](defaultPath.Err())
		}
		path = defaultPath.Unwrap()
	}
	if command == "init" || command == "doctor" || command == "serve" {
		if !commandArgs.IsEmpty() {
			return g.Err[g.Int](errors.New("this command accepts no flags; place --config before the command"))
		}
	}
	if command == "init" {
		created := dialer.InitConfig(path)
		if created.IsErr() {
			return g.Err[g.Int](created.Err())
		}
		return writeJSON(out, g.Map[g.String, any]{"created": true, "config_path": path})
	}
	loaded := dialer.LoadConfig(path)
	if loaded.IsErr() {
		return g.Err[g.Int](loaded.Err())
	}
	cfg := loaded.Unwrap()
	switch command {
	case "doctor":
		missing := cfg.Missing()
		if base := operatorBase(cfg.Listen); base.IsErr() {
			missing.Push("listen must use a loopback address and port 1-65535")
		}
		written := writeJSON(out, g.Map[g.String, any]{
			"config_path": path, "configured": missing.IsEmpty(), "missing": missing,
			"live_verified": false, "message": "Local configuration checks only; provider access, callbacks and call audio are unverified.",
		})
		if written.IsErr() {
			return written
		}
		if !missing.IsEmpty() {
			return g.Ok(g.Int(1))
		}
		return g.Ok(g.Int(0))
	case "serve":
		result := serve(cfg, out)
		if result.IsErr() {
			return g.Err[g.Int](errors.New(redactValue(cfg, result.Err().Error()).(g.String).Std()))
		}
		return result
	default:
		return operatorCommand(command, commandArgs, cfg, out)
	}
}

func help(out io.Writer) g.Result[g.Int] {
	return writeJSON(out, g.Map[g.String, any]{
		"usage": "concierge-dialer [--config PATH] COMMAND [FLAGS]",
		"commands": g.SliceOf[g.String](
			"init", "doctor", "serve", "call --request PATH", "status --id ID",
			"reply --id ID --question-id ID --answer-file PATH",
			"dtmf --id ID --digits DIGITS --command-id UUID [--duration 250]",
			"hangup --id ID", "reconcile --id ID",
			"install --target codex|claude|both --source PATH [--home PATH]",
		),
	})
}

func flags(name g.String) *flag.FlagSet {
	set := flag.NewFlagSet(name.Std(), flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func stdArgs(args g.Slice[g.String]) []string {
	std := g.Slice[string]{}
	args.Iter().ForEach(func(arg g.String) { std.Push(arg.Std()) })
	return std.Std()
}

func operatorCommand(command g.String, args g.Slice[g.String], cfg dialer.Config, out io.Writer) g.Result[g.Int] {
	set := flags(command)
	var id, requestPath, questionID, answerPath, digits, commandID g.String
	duration := g.Int(250)
	setString := func(name g.String, value *g.String) {
		set.Func(name.Std(), "", func(s string) error { *value = g.String(s); return nil })
	}
	if command == "call" {
		setString("request", &requestPath)
	} else {
		setString("id", &id)
	}
	if command == "reply" {
		setString("question-id", &questionID)
		setString("answer-file", &answerPath)
	}
	if command == "dtmf" {
		setString("digits", &digits)
		setString("command-id", &commandID)
		set.Func("duration", "milliseconds", func(s string) error {
			var parsed g.Int
			if err := json.Unmarshal(g.String(s).Bytes(), &parsed); err != nil {
				return errors.New("duration must be an integer")
			}
			duration = parsed
			return nil
		})
	}
	if err := set.Parse(stdArgs(args)); err != nil || set.NArg() != 0 {
		return g.Err[g.Int](errors.New("invalid command flags; use help"))
	}
	method := g.String(http.MethodPost)
	endpoint := g.String("/v1/calls")
	var payload any
	if command == "call" {
		body := readPrivate(requestPath, 65536)
		if body.IsErr() {
			return g.Err[g.Int](body.Err())
		}
		var request dialer.Request
		decoder := json.NewDecoder(bytes.NewReader(body.Unwrap()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return g.Err[g.Int](errors.New("request must contain id, to, task and information as JSON strings"))
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return g.Err[g.Int](errors.New("request must contain exactly one JSON object"))
		}
		if valid := request.Validate(); valid.IsErr() {
			return g.Err[g.Int](valid.Err())
		}
		payload = request
	} else {
		if !callIDPattern.MatchString(id.Std()) {
			return g.Err[g.Int](errors.New("--id must contain 8-80 letters, digits, underscores or hyphens"))
		}
		endpoint += "/" + id
		switch command {
		case "status":
			method = http.MethodGet
		case "reply":
			if questionID.Trim().IsEmpty() {
				return g.Err[g.Int](errors.New("--question-id is required; use the current status question.id"))
			}
			body := readPrivate(answerPath, 16000)
			if body.IsErr() {
				return g.Err[g.Int](body.Err())
			}
			answer := g.String(body.Unwrap()).Trim()
			if answer.IsEmpty() || !utf8.Valid(body.Unwrap()) {
				return g.Err[g.Int](errors.New("answer file must contain nonempty UTF-8 text"))
			}
			payload = g.Map[g.String, any]{"question_id": questionID, "answer": answer}
		case "dtmf":
			if digits.IsEmpty() || commandID.IsEmpty() {
				return g.Err[g.Int](errors.New("--digits and a stable --command-id UUID are required"))
			}
			payload = g.Map[g.String, any]{"digits": digits, "duration_millis": duration, "command_id": commandID}
		case "hangup", "reconcile":
			payload = g.NewMap[g.String, any]()
		default:
			return g.Err[g.Int](errors.New("unknown operator command"))
		}
		if command != "status" {
			endpoint += "/" + command
		}
	}
	return requestOperator(cfg, method, endpoint, payload, out)
}

func readPrivate(path g.String, limit g.Int) g.Result[g.Bytes] {
	if path.IsEmpty() {
		return g.Err[g.Bytes](errors.New("a private input file is required"))
	}
	info, err := os.Lstat(path.Std())
	if err != nil {
		return g.Err[g.Bytes](err)
	}
	if !info.Mode().IsRegular() {
		return g.Err[g.Bytes](errors.New("input must be a regular file, not a symlink or pipe"))
	}
	file, err := os.Open(path.Std())
	if err != nil {
		return g.Err[g.Bytes](err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return g.Err[g.Bytes](err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return g.Err[g.Bytes](errors.New("input must be a private regular file (chmod 600)"))
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return g.Err[g.Bytes](err)
	}
	if g.Int(len(data)) > limit {
		return g.Err[g.Bytes](errors.New("input file is too large"))
	}
	return g.Ok(g.Bytes(data))
}

func operatorBase(listen g.String) g.Result[g.String] {
	host, port, err := net.SplitHostPort(listen.Std())
	if err != nil {
		return g.Err[g.String](errors.New("listen must be a loopback host:port"))
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	var portNumber g.Int
	if ip == nil || !ip.IsLoopback() || json.Unmarshal(g.String(port).Bytes(), &portNumber) != nil || portNumber < 1 || portNumber > 65535 {
		return g.Err[g.String](errors.New("listen must use a loopback address and port 1-65535"))
	}
	return g.Ok(g.String("http://" + net.JoinHostPort(host, port)))
}

func requestOperator(cfg dialer.Config, method, endpoint g.String, payload any, out io.Writer) g.Result[g.Int] {
	base := operatorBase(cfg.Listen)
	if base.IsErr() {
		return g.Err[g.Int](base.Err())
	}
	if cfg.APIToken.Trim().IsEmpty() {
		return g.Err[g.Int](errors.New("api_token is required"))
	}
	var body g.Bytes
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return g.Err[g.Int](errors.New("could not encode request"))
		}
		body = encoded
	}
	request, err := http.NewRequest(method.Std(), (base.Unwrap() + endpoint).Std(), bytes.NewReader(body))
	if err != nil {
		return g.Err[g.Int](errors.New("could not construct operator request"))
	}
	request.Header.Set("Authorization", "Bearer "+cfg.APIToken.Std())
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return g.Err[g.Int](errors.New("operator request failed; inspect status with the same call ID before retrying; never create a new ID to bypass uncertainty"))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil || len(data) > 2*1024*1024 {
		return g.Err[g.Int](errors.New("operator response could not be read; inspect status with the same call ID"))
	}
	var result any
	if err := json.Unmarshal(data, &result); err != nil {
		return g.Err[g.Int](fmt.Errorf("operator returned HTTP %d with invalid JSON; inspect status with the same call ID", response.StatusCode))
	}
	result = redactValue(cfg, result)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		encoded, _ := json.Marshal(result)
		return g.Err[g.Int](fmt.Errorf("operator returned HTTP %d: %s", response.StatusCode, encoded))
	}
	return writeJSON(out, result)
}

func redactValue(cfg dialer.Config, value any) any {
	switch typed := value.(type) {
	case string:
		text := g.String(typed)
		g.SliceOf(cfg.OpenAIKey, cfg.TelnyxKey, cfg.APIToken).Iter().ForEach(func(secret g.String) {
			if !secret.IsEmpty() {
				text = text.ReplaceAll(secret, "[redacted]")
			}
		})
		return text
	case []any:
		return g.Slice[any](typed).Iter().Map(func(item any) any { return redactValue(cfg, item) }).Collect()
	case map[string]any:
		result := g.NewMap[g.String, any]()
		g.Map[string, any](typed).Iter().ForEach(func(key string, item any) {
			result[redactValue(cfg, key).(g.String)] = redactValue(cfg, item)
		})
		return result
	default:
		return value
	}
}

func writeJSON(out io.Writer, value any) g.Result[g.Int] {
	if err := json.NewEncoder(out).Encode(value); err != nil {
		return g.Err[g.Int](err)
	}
	return g.Ok(g.Int(0))
}

func serve(cfg dialer.Config, out io.Writer) g.Result[g.Int] {
	base := operatorBase(cfg.Listen)
	if base.IsErr() {
		return g.Err[g.Int](base.Err())
	}
	listener, err := net.Listen("tcp", base.Unwrap().ReplaceAll("http://", "").Std())
	if err != nil {
		return g.Err[g.Int](err)
	}
	defer listener.Close()
	created := dialer.NewServer(cfg)
	if created.IsErr() {
		return g.Err[g.Int](created.Err())
	}
	service := created.Unwrap()
	server := &http.Server{Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	written := writeJSON(out, g.Map[g.String, any]{"listening": listener.Addr().String(), "service_ready": true, "live_verified": false})
	if written.IsErr() {
		stop()
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-finished:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serviceErr := service.Shutdown(shutdownCtx)
	httpErr := server.Shutdown(shutdownCtx)
	if written.IsErr() {
		return written
	}
	if serviceErr != nil {
		return g.Err[g.Int](serviceErr)
	}
	if httpErr != nil {
		return g.Err[g.Int](httpErr)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return g.Err[g.Int](serveErr)
	}
	return g.Ok(g.Int(0))
}
