/*

 Moses is getraind met deze escapes:

    & -> &amp;
    | -> &#124;


 TODO: rotate log

*/

package main

import (
	"github.com/pebbe/tokenize"

	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	SH            = "/bin/sh"
	PATH          = "/bin:/usr/bin:/net/aps/64/bin"
	TRUECASEMODEL = "corpus/data/truecase-model"
	TOKENIZER     = "/net/aps/64/opt/moses/mosesdecoder/scripts/tokenizer/tokenizer.perl"
	POOLSIZE      = 12   // gelijk aan aantal threads in elk van de twee mosesservers
	QUEUESIZE     = 1000 // inclusief request die nu verwerkt worden
)

var (
	rePar      = regexp.MustCompile("\n\\s*\n")
	rePunct    = regexp.MustCompile(`^[.!?]+$`)
	reEndPoint = regexp.MustCompile(`\pL\pL\pP*[.!?]\s*$`)
	reMidPoint = regexp.MustCompile(`\p{Ll}\p{Ll}\pP*[.!?]\s+('s\s+|'t\s+)?\p{Lu}`)
	reLet      = regexp.MustCompile(`\pL`)
	reLetNum   = regexp.MustCompile(`\pL|\pN`)

	best  = make(map[string]map[string]string)
	known = make(map[string]map[string]bool)
)

////////////////////////////////////////////////////////////////

type CallT struct {
	XMLName    xml.Name  `xml:"methodCall"`
	MethodName string    `xml:"methodName"`
	Params     []MemberT `xml:"params>param>value>struct>member"`
}

type ResponseT struct {
	XMLName xml.Name  `xml:"methodResponse"`
	Params  []MemberT `xml:"params>param>value>struct>member,omitempty"`
	Fault   []MemberT `xml:"fault>value>struct>member,omitempty"`
}

type ValueT struct {
	I4        *int      `xml:"i4,omitempty"`
	Int       *int      `xml:"int,omitempty"`
	IntVal    string    `xml:"intval,omitempty"`
	Boolean   int       `xml:"boolean,omitempty"`
	BoolVal   string    `xml:"boolval,omitempty"`
	String    string    `xml:"string,omitempty"`
	Text      string    `xml:",chardata""`
	Double    float64   `xml:"double,omitempty"`
	DoubleVal string    `xml:"doubleval,omitempty"`
	Struct    []MemberT `xml:"struct>member,omitempty"`
	Array     []ValueT  `xml:"array>data>value,omitempty"`
}

type MemberT struct {
	Name  string `xml:"name"`
	Value ValueT `xml:"value"`
}

////////////////////////////////////////////////////////////////

type MosesT struct {
	mt     *ResponseT
	tok    string
	err    string
	errnum int
}

////////////////////////////////////////////////////////////////

type Json struct {
	ErrorCode    int            `json:"errorCode"`
	ErrorMessage string         `json:"errorMessage"`
	TimeWait     string         `json:"timeWait"`
	TimeWork     string         `json:"timeWork"`
	Translation  []TranslationT `json:"translation"`
}

type TranslationT struct {
	ErrorCode    int           `json:"errorCode,omitempty"`
	ErrorMessage string        `json:"errorMessage,omitempty"`
	Src          string        `json:"src,omitempty"`
	SrcTokenized string        `json:"src-tokenized,omitempty"`
	Translated   []TranslatedT `json:"translated,omitempty"`
}

type TranslatedT struct {
	Text         string          `json:"text,omitempty"`
	Score        float64         `json:"score"`
	Rank         int             `json:"rank"`
	Tokenized    string          `json:"tokenized,omitempty"`
	AlignmentRaw []AlignmentRawT `json:"alignment-raw,omitempty"`
}

type AlignmentRawT struct {
	SrcStart int `json:"src-start"`
	SrcEnd   int `json:"src-end"`
	TgtStart int `json:"tgt-start"`
	TgtEnd   int `json:"tgt-end"`
}

////////////////////////////////////////////////////////////////

type Request struct {
	Action        string `json:"action"`
	SourceLang    string `json:"sourceLang"`
	TargetLang    string `json:"targetLang"`
	AlignmentInfo bool   `json:"alignmentInfo"`
	NBestSize     int    `json:"nBestSize"`
	Detokenize    bool   `json:"detokenize"`
	Text          string `json:"text"`
}

////////////////////////////////////////////////////////////////

type Result struct {
	resp   []byte
	err    error
	srcTok string
}

////////////////////////////////////////////////////////////////

var (
	chPoolNL = make(chan bool, POOLSIZE)
	chPoolEN = make(chan bool, POOLSIZE)
	chQueue  = make(chan bool, QUEUESIZE*2)
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	for _, lang := range []string{"nl", "en"} {
		best[lang] = make(map[string]string)
		known[lang] = make(map[string]bool)
		fp, err := os.Open(TRUECASEMODEL + "." + lang)
		if err != nil {
			log.Fatal(err)
		}
		scanner := bufio.NewScanner(fp)
		for scanner.Scan() {
			aa := strings.Fields(scanner.Text())
			if len(aa) > 0 && reLet.MatchString(aa[0]) {
				best[lang][strings.ToLower(aa[0])] = aa[0]
				known[lang][aa[0]] = true
				for _, a := range aa[1:] {
					if reLet.MatchString(a) {
						known[lang][a] = true
					}
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
		fp.Close()
	}

	http.HandleFunc("/", info)
	http.HandleFunc("/rpc", handleJson)
	http.HandleFunc("/xmlrpc", handleXml)
	http.HandleFunc("/favicon.ico", favicon)
	http.HandleFunc("/robots.txt", robots)

	log.Print("Server starting")
	log.Print("Server exit: ", http.ListenAndServe(":9070", nil))
}

func handleJson(w http.ResponseWriter, r *http.Request) {
	handle(w, r, false)
}

func handleXml(w http.ResponseWriter, r *http.Request) {
	handle(w, r, true)
}

func handle(w http.ResponseWriter, r *http.Request, xmlrpc bool) {

	if len(chQueue) >= QUEUESIZE {
		http.Error(w, "Too many requests", http.StatusServiceUnavailable)
		return
	}

	start1 := time.Now()

	log.Printf("[%s] %s %s %s", r.Header.Get("X-Forwarded-For"), r.RemoteAddr, r.Method, r.URL.Path)

	var chClose <-chan bool
	if f, ok := w.(http.CloseNotifier); ok {
		chClose = f.CloseNotify()
	} else {
		chClose = make(<-chan bool)
	}

	req := &Request{
		AlignmentInfo: false,
		NBestSize:     1,
		Detokenize:    true,
	}

	if xmlrpc {

		w.Header().Set("Content-Type", "txt/xml")

		b, _ := ioutil.ReadAll(r.Body)
		r.Body.Close()

		call := &CallT{}
		err := xml.Unmarshal(b, call)
		if err != nil {
			rerror(w, xmlrpc, 5, "Parse error: "+err.Error())
			return
		}

		switch call.MethodName {
		case "alive_check":
			fmt.Fprint(w, `<?xml version='1.0'?>
<methodResponse>
<params>
<param>
<value><int>1</int></value>
</param>
</params>
</methodResponse>
`)
			return
		case "process_task":
		default:
			rerror(w, xmlrpc, 5, "Method \""+call.MethodName+"\" is not supported")
			return
		}

		for _, p := range call.Params {
			switch p.Name {
			case "action":
				req.Action = xmlString(p)
			case "sourceLang":
				req.SourceLang = xmlString(p)
			case "targetLang":
				req.TargetLang = xmlString(p)
			case "alignmentInfo":
				req.AlignmentInfo = xmlBool(p)
			case "text":
				req.Text = xmlString(p)
			case "nBestSize":
				req.NBestSize = xmlInt(p)
			case "detokenize":
				req.Detokenize = xmlBool(p)
			}
		}

	} else {

		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			r.ParseForm()
			req.Action = first(r, "action")
			req.SourceLang = first(r, "sourceLang")
			req.TargetLang = first(r, "targetLang")
			req.Text = first(r, "text")
			if first(r, "alignmentInfo") == "true" {
				req.AlignmentInfo = true
			}
			if first(r, "detokenize") == "false" {
				req.Detokenize = false
			}
			if n, err := strconv.Atoi(first(r, "nBestSize")); err == nil {
				req.NBestSize = n
			}
		case "POST":
			w.Header().Set("Content-Type", "application/json")
			b, _ := ioutil.ReadAll(r.Body)
			r.Body.Close()
			err := json.Unmarshal(b, req)
			if err != nil {
				rerror(w, xmlrpc, 5, "Parse error: "+err.Error())
				return
			}
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

	}

	chQueue <- true
	defer func() { <-chQueue }()

	if req.Action != "translate" {
		rerror(w, xmlrpc, 5, "value of 'action' should be 'translate")
		return
	}

	if req.SourceLang == "en" && req.TargetLang == "nl" {
		chPoolEN <- true
		defer func() {
			<-chPoolEN
		}()
	} else if req.SourceLang == "nl" && req.TargetLang == "en" {
		chPoolNL <- true
		defer func() {
			<-chPoolNL
		}()
	} else {
		rerror(w, xmlrpc, 3, "Invalid combination of SourceLang + TargetLang")
		return
	}

	start2 := time.Now()
	time1 := start2.Sub(start1)

	select {
	case <-chClose:
		log.Print("Request dropped")
		return
	default:
	}

	parts := strings.Split(rePar.ReplaceAllLiteralString(req.Text, "\n\n"), "\n\n")
	lines := make([]string, 0)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		run := isRun(strings.Split(part, "\n"))
		if run {
			part = strings.Replace(part, "\n", " ", -1)
		}

		var ss string
		var err error
		if req.SourceLang == "nl" {
			ss, err = tokenize.Dutch(part, run)
			if err != nil {
				rerror(w, xmlrpc, 8, "Tokenizer: "+err.Error())
				return
			}
			ss = strings.TrimSpace(ss)
		} else {
			ss, err = doCmd("echo %s | %s -l en", quote(part), TOKENIZER)
			if err != nil {
				rerror(w, xmlrpc, 8, "Tokenizer: "+err.Error())
				return
			}
			ss = html.UnescapeString(ss)
			if run {
				ss = splitlines(part, ss)
			}
		}

		// here we escape '&' and '|' because those were the escapes in the training data
		ss = escape(ss)

		for _, s := range strings.Split(ss, "\n") {
			s = strings.TrimSpace(s)
			if s != "" {

				// truecase
				words := strings.Fields(s)
				sentence_start := true
				for i, word := range words {
					lcword := strings.ToLower(word)
					if w, ok := best[req.SourceLang][lcword]; sentence_start && ok {
						// truecase sentence start
						words[i] = w
					} else if known[req.SourceLang][word] {
						// don't change known words
					} else if w, ok := best[req.SourceLang][lcword]; ok {
						// truecase otherwise unknown words
						words[i] = w
					}
					switch sentence_start {
					case false:
						if word == ":" || rePunct.MatchString(word) {
							sentence_start = true
						}
					case true:
						if reLetNum.MatchString(word) && word != "'s" && word != "'t" {
							sentence_start = false
						}
					}
				}
				s = strings.Join(words, " ")

				lines = append(lines, s)
			}
		}
	}

	if req.NBestSize < 1 {
		req.NBestSize = 1
	} else if req.NBestSize > 10 {
		req.NBestSize = 10
	}

	responses := make([]*MosesT, 0)
	for _, line := range lines {
		rs := &MosesT{
			mt:  &ResponseT{},
			tok: line,
		}
		if n := len(strings.Fields(line)); n > 100 {
			rs.err = "Line has more than 100 words (after tokenisation)"
			rs.errnum = 5
		} else if n == 0 {
			rs.err = "Missing text"
			rs.errnum = 5
		} else {
			resp, err := doMoses(req.SourceLang, line, req.AlignmentInfo, req.NBestSize)
			if err != nil {
				log.Print("Moses: ", err)
				rs.err = err.Error()
				rs.errnum = 8
			} else {
				if err := xml.Unmarshal(resp, rs.mt); err != nil {
					panic(err)
				}
				if rs.mt.Fault != nil {
					rs.err = "unknown"
					rs.errnum = 8
					for _, member := range rs.mt.Fault {
						if member.Name == "faultString" {
							if member.Value.String != "" {
								rs.err = member.Value.String
							} else {
								rs.err = member.Value.Text
							}
						}
					}
				}
			}
		}
		responses = append(responses, rs)
	}

	js := decodeMulti(responses, req.Detokenize, req.AlignmentInfo, req.TargetLang)

	if req.AlignmentInfo {
		for _, t := range js.Translation {
			for _, tt := range t.Translated {
				if n := len(tt.AlignmentRaw); n > 0 {
					for i := 0; i < n-1; i++ {
						tt.AlignmentRaw[i].TgtEnd = tt.AlignmentRaw[i+1].TgtStart - 1
					}
					tt.AlignmentRaw[n-1].TgtEnd = len(strings.Fields(tt.Tokenized)) - 1
				}
			}
		}
	}

	time2 := time.Now().Sub(start2)
	js.TimeWait = time1.String()
	js.TimeWork = time2.String()

	log.Printf("Requests: %d - Wait: %v - Work: %v - Lines: %d", len(chQueue), time1, time2, len(lines))

	if xmlrpc {
		fmt.Fprintln(w, "<?xml version='1.0' encoding='UTF-8'?>\n<methodResponse><params><param><value>")
		marshall(reflect.ValueOf(*js), w)
		fmt.Fprintln(w, "</value></param></params></methodResponse>")
	} else {
		b, _ := json.MarshalIndent(js, "", "  ")
		fmt.Fprintln(w, string(b))
	}
}

func marshall(r reflect.Value, w http.ResponseWriter) {
	switch k := r.Kind(); k {
	case reflect.Struct:
		fmt.Fprintln(w, "<struct>")
		t := r.Type()
		for i := 0; i < r.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			s := strings.Split(tag, ",")
			var n string
			omitempty := false
			if len(s) > 0 {
				n = s[0]
				for _, opt := range s[1:] {
					if opt == "omitempty" {
						omitempty = true
					}
				}
			} else {
				n = f.Name
			}
			r2 := r.Field(i)
			if omitempty && isempty(r2) {
				continue
			}
			fmt.Fprintln(w, "<member>")
			fmt.Fprintf(w, "<name>%s</name>\n", html.EscapeString(n))
			fmt.Fprintln(w, "<value>")
			marshall(r2, w)
			fmt.Fprintln(w, "</value>")
			fmt.Fprintln(w, "</member>")
		}
		fmt.Fprintln(w, "</struct>")
	case reflect.Int:
		fmt.Fprintf(w, "<int>%d</int>\n", r.Int())
	case reflect.Float64:
		fmt.Fprintf(w, "<double>%g</double>\n", r.Float())
	case reflect.String:
		fmt.Fprintf(w, "<string>%s</string>\n", r.String())
	case reflect.Slice:
		fmt.Fprintln(w, "<array><data>")
		for i := 0; i < r.Len(); i++ {
			fmt.Fprintln(w, "<value>")
			marshall(r.Index(i), w)
			fmt.Fprintln(w, "</value>")
		}
		fmt.Fprintln(w, "</data></array>")
	default:
		panic(fmt.Errorf("unknown type: %s", k))
	}
}

func isempty(r reflect.Value) bool {
	switch k := r.Kind(); k {
	case reflect.Struct:
		t := r.Type()
		for i := 0; i < r.NumField(); i++ {
			s := strings.Split(t.Field(i).Tag.Get("json"), ",")
			omitempty := false
			if len(s) > 0 {
				for _, opt := range s[1:] {
					if opt == "omitempty" {
						omitempty = true
					}
				}
			}
			if !omitempty {
				return false
			}
			if !isempty(r.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Int:
		if r.Int() == 0 {
			return true
		}
	case reflect.Float64:
		if r.Float() == 0 {
			return true
		}
	case reflect.String:
		if r.String() == "" {
			return true
		}
	case reflect.Bool:
		return r.Bool()
	case reflect.Slice:
		for i := 0; i < r.Len(); i++ {
			if !isempty(r.Index(i)) {
				return false
			}
		}
		return true
	default:
		panic(fmt.Errorf("unknown type: %s", k))
	}
	return false
}

func splitlines(ori, tok string) string {
	tt := strings.Fields(tok)
	out := make([]string, len(tt))
	for i, t := range tt {
		ori = strings.TrimSpace(ori)
		if len(ori) > len(t) && ori[len(t)] == ' ' && rePunct.MatchString(t) {
			out[i] = t + "\n"
		} else {
			out[i] = t + " "
		}
		if n := len(t); len(ori) > n {
			ori = ori[n:]
		}
	}
	return strings.TrimSpace(strings.Join(out, ""))
}

func isRun(lines []string) bool {
	ln := float64(len(lines))
	if ln < 2 {
		return true
	}
	midpoint := float64(0)
	endletter := float64(0)
	for _, line := range lines {
		if !reEndPoint.MatchString(line) {
			endletter += 1
		}
		midpoint += float64(len(reMidPoint.FindAllString(line, -1)))
	}
	if endletter > ln*.3 || midpoint > ln*.3 {
		return true
	}
	return false
}

func decodeMulti(responses []*MosesT, dodetok, doalign bool, tgtlang string) *Json {

	repl := &Json{
		Translation:  make([]TranslationT, len(responses)),
		ErrorCode:    0,
		ErrorMessage: "OK",
	}

	/*
		var srclang string
		if tgtlang == "nl" {
			srclang = "en"
		} else {
			srclang = "nl"
		}
	*/

	for idx, resp := range responses {

		if resp.errnum != 0 || resp.err != "" {
			repl.ErrorCode = 99
			repl.ErrorMessage = "Failed to translate some sentence(s)"
			repl.Translation[idx].ErrorCode = resp.errnum
			repl.Translation[idx].ErrorMessage = resp.err
			if resp.errnum == 0 {
				repl.Translation[idx].ErrorCode = 99
			}
			continue
		}

		repl.Translation[idx].Translated = make([]TranslatedT, 0)

		if doalign || len(responses) > 1 {
			repl.Translation[idx].SrcTokenized = strings.TrimSpace(unescape(resp.tok))
		}
		if len(responses) > 1 {
			// repl.Translation[idx].Src = untok(repl.Translation[idx].SrcTokenized, srclang)
		}

		var nbest []ValueT
		for _, member := range resp.mt.Params {
			if member.Name == "nbest" {
				nbest = member.Value.Array
			}
		}

		for i, translated := range nbest {
			tr := TranslatedT{
				Rank: i,
			}
			for _, member := range translated.Struct {
				switch member.Name {
				case "align":
					tr.AlignmentRaw = make([]AlignmentRawT, 0)
					for _, v := range member.Value.Array {
						a := AlignmentRawT{
							SrcStart: -1,
							SrcEnd:   -1,
							TgtStart: -1,
							TgtEnd:   -1,
						}
						for _, member := range v.Struct {
							var value int
							if member.Value.I4 != nil {
								value = *member.Value.I4
							} else if member.Value.Int != nil {
								value = *member.Value.Int
							}
							switch member.Name {
							case "src-end":
								a.SrcEnd = value
							case "src-start":
								a.SrcStart = value
							case "tgt-start":
								a.TgtStart = value
							}
						}
						tr.AlignmentRaw = append(tr.AlignmentRaw, a)
					}
				case "hyp":
					tr.Tokenized = strings.TrimSpace(unescape(member.Value.String))
					if dodetok {
						tr.Text = untok(tr.Tokenized, tgtlang)
					} else {
						tr.Text = tr.Tokenized
					}
				case "totalScore":
					tr.Score = member.Value.Double
				}
			}
			if !doalign {
				tr.Tokenized = ""
			}
			repl.Translation[idx].Translated = append(repl.Translation[idx].Translated, tr)
		}
	}
	return repl
}

func first(r *http.Request, opt string) string {
	if len(r.Form[opt]) > 0 {
		return strings.TrimSpace(r.Form[opt][0])
	}
	return ""
}

func xmlString(p MemberT) string {
	if p.Value.String != "" {
		return p.Value.String
	}
	return p.Value.Text
}

func xmlBool(p MemberT) bool {
	return p.Value.Boolean != 0
}

func xmlInt(p MemberT) int {
	if p.Value.Int != nil {
		return *p.Value.Int
	}
	if p.Value.I4 != nil {
		return *p.Value.I4
	}
	return 0
}

func rerror(w http.ResponseWriter, xmlrpc bool, code int, msg string) {
	if xmlrpc {
		fmt.Fprintf(w, `<?xml version='1.0' encoding='UTF-8'?>
<methodResponse>
  <fault>
    <value>
      <struct>
        <member>
          <name>faultCode</name>
          <value><int>%d</int></value>
        </member>
        <member>
          <name>faultString</name>
          <value><string>%s</string></value>
        </member>
      </struct>
    </value>
  </fault>
</methodResponse>
`, code, html.EscapeString(msg))
	} else {
		fmt.Fprintf(w, `{
    "errorCode": %d,
    "errorMessage": %q
}
`, code, msg)
	}
}

func quote(s string) string {
	return "'" + strings.Replace(s, "'", "'\\''", -1) + "'"
}

func doCmd(format string, a ...interface{}) (string, error) {
	cmd := exec.Command(SH, "-c", fmt.Sprintf(format, a...))
	cmd.Env = []string{
		"PATH=" + PATH,
		"LANG=en_US.utf8",
		"LANGUAGE=en_US.utf8",
		"LC_ALL=en_US.utf8",
	}
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	err = cmd.Start()
	if err != nil {
		return "", err
	}
	b, _ := ioutil.ReadAll(pipe)
	pipe.Close()
	err = cmd.Wait()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func untrue(s, lang string) string {
	// invoer was 1 zin, maar kan vertaald zijn naar meerdere zinnen
	inwords := strings.Fields(s)
	outwords := make([]string, len(inwords))
	state := 1 // need cap
	for i, word := range inwords {
		switch state {
		case 0: // normal
			outwords[i] = word
			if rePunct.MatchString(word) {
				state = 1
			} else if word == ":" && i < len(inwords)-1 && strings.Contains(`"”„“`, inwords[i+1]) {
				state = 1
			}
		case 1: // need cap
			if lang == "nl" {
				if word == "'s" || word == "'t" {
					outwords[i] = word
					break
				}
				if strings.HasPrefix(word, "ij") {
					outwords[i] = "IJ" + word[2:]
					state = 0
					break
				}
			}
			var w string
			for j, c := range word { // use range in case first char is multibyte
				if j == 0 && unicode.IsLetter(c) {
					w = string(unicode.ToUpper(c))
					state = 0
				} else if j == 0 && unicode.IsNumber(c) {
					w = string(c)
					state = 0
				} else {
					w += word[j:]
					break
				}
			}
			outwords[i] = w
		}
	}
	return strings.Join(outwords, " ")
}

func untok(s, lang string) string {

	words := strings.Fields(untrue(s, lang))
	doubO := false
	singO := false
	for i, word := range words {
		if utf8.RuneCountInString(word) == 1 {
			if strings.Contains(".,:;!?)]}", word) {
				words[i] = "\a" + word
				continue
			}
			if strings.Contains("([{", word) {
				words[i] = word + "\a"
				continue
			}
			if strings.Contains("\\/", word) {
				words[i] = "\a" + word + "\a"
				continue
			}
			if strings.Contains("'`’‘‚", word) {
				if singO {
					words[i] = "\a" + word
				} else {
					words[i] = word + "\a"
				}
				singO = !singO
				continue
			}
			if strings.Contains(`"”„“`, word) {
				if doubO {
					words[i] = "\a" + word
				} else {
					words[i] = word + "\a"
				}
				doubO = !doubO
				continue
			}
		}
		if lang == "en" && word[0] == '\'' {
			words[i] = "\a" + word
			continue
		}
		if rePunct.MatchString(word) {
			words[i] = "\a" + word
			continue
		}
	}

	s = strings.Join(words, " ")
	s = strings.Replace(s, " \a", "", -1)
	s = strings.Replace(s, "\a ", "", -1)
	s = strings.Replace(s, "\a", "", -1)
	return s

}

func doMoses(sourceLang, tokenized string, alignmentInfo bool, nBestSize int) ([]byte, error) {
	port := "9071"
	if sourceLang == "nl" {
		port = "9072"
	}

	r := &CallT{
		MethodName: "translate",
		Params: []MemberT{
			MemberT{
				Name: "text",
				Value: ValueT{
					String: tokenized,
				},
			},
			MemberT{
				Name: "nbest",
				Value: ValueT{
					I4: &nBestSize,
				},
			},
			MemberT{
				Name: "nbest-distinct",
				Value: ValueT{
					BoolVal: "1",
				},
			},
		},
	}

	if alignmentInfo {
		r.Params = append(r.Params,
			MemberT{
				Name: "align",
				Value: ValueT{
					BoolVal: "1",
				},
			})
	}

	m, _ := xml.MarshalIndent(r, "", "  ")
	s := fixxml(string(m))

	var buf bytes.Buffer
	buf.WriteString("<?xml version='1.0' encoding='UTF-8'?>\n")
	buf.WriteString(s)

	resp, err := http.Post("http://127.0.0.1:"+port+"/RPC2", "text/xml", &buf)

	if err != nil {
		return []byte{}, err
	}

	b, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	return b, nil
}

var (
	reEmpty  = regexp.MustCompile(` *<[a-zA-Z]+>\s*</[a-zA-Z]+> *\n?`)
	reInt    = regexp.MustCompile(`<intval>(-?[0-9]+)</intval>`)
	reBool   = regexp.MustCompile(`<boolval>([0-9]+)</boolval>`)
	reDouble = regexp.MustCompile(`<doubleval>(.*?)</doubleval>`)
)

func fixxml(s string) string {
	s1 := ""
	for s != s1 {
		s1 = s
		s = reEmpty.ReplaceAllString(s, "")
	}
	s = reInt.ReplaceAllString(s, "<int>$1</int>")
	s = reBool.ReplaceAllString(s, "<boolean>$1</boolean>")
	s = reDouble.ReplaceAllString(s, "<double>$1</double>")
	return s
}

func escape(s string) string {
	return strings.Replace(strings.Replace(s, "&", "&amp;", -1), "|", "&#124;", -1)
}

func unescape(s string) string {
	return strings.Replace(strings.Replace(s, "&#124;", "|", -1), "&amp;", "&", -1)
}

////////////////////////////////////////////////////////////////

func info(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	cache(w)
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
  <head>
    <meta name="robots" content="noindex,nofollow">
    <title>QTLeap -- Groningen</title>
  </head>
  <body>
This is the moses based translater between Dutch and English for the <a href="http://qtleap.eu/">QTleap</a> project at the
<a href="http://www.let.rug.nl/~vannoord/">Rijksuniversiteit Groningen</a>.
<p>
Examples:
<pre>
    curl -d '{
        "action":        "translate",
        "sourceLang":    "nl",
        "targetLang":    "en",
        "text":          "Dit is een test. En dit is ook een test.",
        "detokenize":    true,
        "alignmentInfo": true,
        "nBestSize":     3
    }' http://zardoz.service.rug.nl:9070/rpc
</pre>
<p>
or:
<pre>
    <a href="http://zardoz.service.rug.nl:9070/rpc?action=translate&amp;sourceLang=nl&amp;targetLang=en&amp;text=Dit+is+een+test.+En+dit+is+ook+een+test.&amp;detokenize=true&amp;alignmentInfo=true&amp;nBestSize=3">http://zardoz.service.rug.nl:9070/rpc?action=translate&amp;sourceLang=nl&amp;targetLang=en&amp;text=Dit+is+een+test.+En+dit+is+ook+een+test.&amp;detokenize=true&amp;alignmentInfo=true&amp;nBestSize=3</a>
</pre>
<p>
or:
<pre>
    curl -d '&lt;?xml version="1.0" encoding="UTF-8"?&gt;
    &lt;methodCall&gt;
      &lt;methodName&gt;process_task&lt;/methodName&gt;
      &lt;params&gt;&lt;param&gt;&lt;value&gt;&lt;struct&gt;
        &lt;member&gt;&lt;name&gt;action&lt;/name&gt;&lt;value&gt;&lt;string&gt;translate&lt;/string&gt;&lt;/value&gt;&lt;/member&gt;
        &lt;member&gt;&lt;name&gt;sourceLang&lt;/name&gt;&lt;value&gt;&lt;string&gt;nl&lt;/string&gt;&lt;/value&gt;&lt;/member&gt;
        &lt;member&gt;&lt;name&gt;targetLang&lt;/name&gt;&lt;value&gt;&lt;string&gt;en&lt;/string&gt;&lt;/value&gt;&lt;/member&gt;
        &lt;member&gt;&lt;name&gt;text&lt;/name&gt;&lt;value&gt;&lt;string&gt;Dit is een test. En dit is ook een test.&lt;/string&gt;&lt;/value&gt;&lt;/member&gt;
        &lt;member&gt;&lt;name&gt;alignmentInfo&lt;/name&gt;&lt;value&gt;&lt;boolean&gt;1&lt;/boolean&gt;&lt;/value&gt;&lt;/member&gt;
        &lt;member&gt;&lt;name&gt;nBestSize&lt;/name&gt;&lt;value&gt;&lt;i4&gt;3&lt;/i4&gt;&lt;/value&gt;&lt;/member&gt;
      &lt;/struct&gt;&lt;/value&gt;&lt;/param&gt;&lt;/params&gt;
    &lt;/methodCall&gt;' http://zardoz.service.rug.nl:9070/xmlrpc
</pre>
<p>
Alive?
<pre>
    curl -d '&lt;?xml version="1.0" encoding="UTF-8"?&gt;
    &lt;methodCall&gt;
      &lt;methodName&gt;alive_check&lt;/methodName&gt;
    &lt;/methodCall&gt;' http://zardoz.service.rug.nl:9070/xmlrpc
</pre>
See: <a href="https://github.com/ufal/mtmonkey/blob/master/API.md">API</a>
<p>
Sources: <a href="https://github.com/rug-compling/qtleap/tree/master/moses">github</a>
  </body>
</html>
`)

}

////////////////////////////////////////////////////////////////

var file__favicon__ico = `
AAABAAEAEBAAAAEACABoBQAAFgAAACgAAAAQAAAAIAAAAAEACAAAAAAAAAEAAAAAAAAAAAAAAAEAAAAA
AAD19fwA7OzkANLSywD5+fkA39/gAOrq6gD3+PQAqab/APf39wDt7O0A09PJAO/v7QDV1dQA4uH0AOjo
4ADb2ucAqKT9APX19QDm5uYAzMzNANfX1wDf3v0A5OP3AOno8QDX2MwAioX7AJaS9wDz8/MA09PKANnZ
2gDk5OQA9fX+AOnp3gDm5u8A5eXZAM7OyADx8fEAu7j/AOnq5ADNzcMAn5v3ANrZ/ADPz84A+Pf5AJ6a
+gDf39IAzs7RAMjIwQDT08sA/v7+ANrZ2wDk5OUAwr/8AO/v7wDKyswA1dXWANzd2ADR0dEAp6T/AMvK
xACLh/cA8fHyAMzMzwC/vfoA/Pz8AOnq5QDi4uMA0NDMANLR+wC2s/4A09H7ANTUyQD8/PQA7e3lAKKe
+AD8+/cA8O/wAPr6+gDa2tEAiIT4AOvr6wDIyMsAycjLAPj4+ADExMYAycnAANTUygCalvcA1tbVAJqV
+gDr6+wAkIz7APb29gDf3+UA3NzdAOfn5wDZ1/wAm5f9ANjY0AD09PQAxML/APLy8gCLh/oAzc7EAPz8
/wDT0vsAj4v3ANDQzwD19PUA1NTMAP///wDb2twAysj/APDw8ADh4eEAqab9AIqF+AC2s/kAlZH3AP39
/QD6+vUA7u7uAPn5+ADk5NkA0dDQAJiT+gDv7/wAy8n9AIqG9gD7+/sAmpb6APz7+wDs7OwA09PIAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAbm5ubjEDXHpjG4Fubm5ubm5uboEkeWVQcoQIcYNubm5uMVNlUxRDGC1DNyQkU25ubkBxElQw
Jg0WQRxUQiQxbm5lcVEKAAcZZjp+VlIbTDFceR2FSyxKKRUoWStHMhJTbAVYASUabm5ubldFSQx5CFw1
YiFPRG5ubm5pdBdOWhtcJAJdfUZubm5uYIIPbTOEXAk5IH88H25uaIA0DnwbAwM1XmdIc2o/dXYQXCdv
EUBuG0w+LwZwYVtkeFUuAzVAbnc1eTYje18LIjsTJD1cbm5uTTVxBGs4OCpCZYRjbm5ubm53PTUbcR5c
XHFNbm5ubm5ubm4xTRsbQHdubm5ubgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=`

func init() {
	var b []byte
	b, _ = base64.StdEncoding.DecodeString(file__favicon__ico)
	file__favicon__ico = string(b)
}

func favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	cache(w)
	fmt.Fprint(w, file__favicon__ico)
}

////////////////////////////////////////////////////////////////

func robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	cache(w)
	fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
}

////////////////////////////////////////////////////////////////

func cache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
}
