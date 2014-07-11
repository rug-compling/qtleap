/*

 TODO: rotate log

 TODO: '|' in invoer wordt omgezet in '_', is dat OK?

*/

package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	SH          = "/bin/sh"
	PATH        = "/net/aistaff/kleiweg/paqu/bin:/bin:/usr/bin:/net/aps/64/bin"
	ALPINO      = "/net/aps/64/src/Alpino"
	TRUECASER   = "/net/aps/64/opt/moses/mosesdecoder/scripts/recaser/truecase.perl"
	DETRUECASER = "/net/aps/64/opt/moses/mosesdecoder/scripts/recaser/detruecase.perl"
	TOKENIZER   = "/net/aps/64/opt/moses/mosesdecoder/scripts/tokenizer/tokenizer.perl"
	DETOKENIZER = "/net/aps/64/opt/moses/mosesdecoder/scripts/tokenizer/detokenizer.perl"
	POOLSIZE    = 12   // gelijk aan aantal threads in elk van de twee mosesservers
	QUEUESIZE   = 1000 // inclusief request die nu verwerkt worden
)

////////////////////////////////////////////////////////////////

type MethodResponseT struct {
	XMLName xml.Name `xml:"methodResponse"`
	Params  ParamsT  `xml:"params"`
	Fault   []FaultT `xml:"fault"`
}
type FaultT struct {
	Value ValueT `xml:"value"`
}

type ParamsT struct {
	Param ParamT `xml:"param"`
}

type ParamT struct {
	Value ValueT `xml:"value"`
}

type ValueT struct {
	Struct StructT `xml:"struct"`
	String string  `xml:"string"`
	Array  ArrayT  `xml:"array"`
	I4     int     `xml:"i4"`
	Double float64 `xml:"double"`
}

type StructT struct {
	Member []MemberT `xml:"member"`
}

type MemberT struct {
	Name  string `xml:"name"`
	Value ValueT `xml:"value"`
}

type ArrayT struct {
	Data DataT `xml:"data"`
}

type DataT struct {
	Value []ValueT `xml:"value"`
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
	Translated    []TranslatedT `json:"translated"`
	TranslationId string        `json:"translationId"`
}

type TranslatedT struct {
	Text         string          `json:"text,omitempty"`
	Score        float64         `json:"score,omitempty"`
	Rank         int             `json:"rank"`
	TgtTokenized string          `json:"tgt-tokenized"`
	SrcTokenized string          `json:"src-tokenized"`
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
	Tokenized     string
}

////////////////////////////////////////////////////////////////

var (
	chPoolNL = make(chan bool, POOLSIZE)
	chPoolEN = make(chan bool, POOLSIZE)
	chQueue  = make(chan bool, QUEUESIZE*2)
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	http.HandleFunc("/", info)
	http.HandleFunc("/rpc", handle)
	http.HandleFunc("/favicon.ico", favicon)
	http.HandleFunc("/robots.txt", robots)

	log.Println("Server starting")
	log.Println(http.ListenAndServe(":9070", nil))
}

func handle(w http.ResponseWriter, r *http.Request) {

	if len(chQueue) >= QUEUESIZE {
		http.Error(w, "Too many requests", http.StatusServiceUnavailable)
		return
	}
	chQueue <- true
	defer func() { <-chQueue }()

	start1 := time.Now()

	log.Printf("[%s] %s %s %s", r.Header.Get("X-Forwarded-For"), r.RemoteAddr, r.Method, r.URL)

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

	switch r.Method {
	case "GET":
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
		b, _ := ioutil.ReadAll(r.Body)
		r.Body.Close()
		json.Unmarshal(b, req)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if req.Action != "translate" {
		rerror(w, 5, "value of 'action' should be 'translate")
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
		rerror(w, 3, "Invalid combination of SourceLang + TargetLang")
		return
	}

	start2 := time.Now()
	time1 := start2.Sub(start1)

	select {
	case <-chClose:
		log.Println("Request dropped")
		return
	default:
	}

	id5 := md5.New()
	id5.Write([]byte(fmt.Sprint(req)))
	id := fmt.Sprintf("%x", id5.Sum(nil))

	if req.SourceLang == "en" && req.TargetLang == "nl" {
		req.Tokenized = tokEN(req.Text)
	} else {
		req.Tokenized = tokNL(req.Text)
	}
	req.Tokenized = strings.Replace(req.Tokenized, "|", "_", 01)

	if n := len(strings.Fields(req.Tokenized)); n > 100 {
		rerror(w, 5, "Text has more than 100 words (after tokenisation)")
		return
	} else if n == 0 {
		rerror(w, 5, "Missing text")
		return
	}

	if req.NBestSize < 1 {
		req.NBestSize = 1
	} else if req.NBestSize > 10 {
		req.NBestSize = 10
	}

	resp, err := doMoses(req)
	if err != nil {
		rerror(w, 8, err.Error())
		return
	}

	mt := &MethodResponseT{}
	xml.Unmarshal(resp, mt)

	if len(mt.Fault) > 0 {
		err := "unknown"
		for _, member := range mt.Fault[0].Value.Struct.Member {
			if member.Name == "faultString" {
				err = html.EscapeString(member.Value.String)
			}
		}
		rerror(w, 8, "error: "+err)
		return
	}

	var js *Json
	if req.NBestSize == 1 {
		js = decodeUni(mt, req.Tokenized, req.Detokenize, req.TargetLang, id)
	} else {
		js = decodeMulti(mt, req.Tokenized, req.Detokenize, req.TargetLang, id)
	}

	if req.AlignmentInfo {
		for _, t := range js.Translation {
			for _, tt := range t.Translated {
				if n := len(tt.AlignmentRaw); n > 0 {
					for i := 0; i < n-1; i++ {
						tt.AlignmentRaw[i].TgtEnd = tt.AlignmentRaw[i+1].TgtStart - 1
					}
					tt.AlignmentRaw[n-1].TgtEnd = len(strings.Fields(tt.TgtTokenized)) - 1
				}
			}
		}
	}

	time2 := time.Now().Sub(start2)
	js.TimeWait = time1.String()
	js.TimeWork = time2.String()
	b, _ := json.MarshalIndent(js, "", "  ")
	s := string(b)

	if req.NBestSize < 2 {
		s = strings.Replace(s, "\n"+`          "rank": 0,`, "", 1)
	}

	fmt.Fprintf(w, s+"\n")

	log.Printf("Requests: %d - Wait: %v - Process: %v\n", len(chQueue), time1, time2)
}

func decodeUni(resp *MethodResponseT, srctok string, dodetok bool, tgtlang, id string) *Json {

	repl := &Json{
		Translation:  make([]TranslationT, 1),
		ErrorMessage: "OK",
	}
	repl.Translation[0].Translated = make([]TranslatedT, 1)
	repl.Translation[0].TranslationId = id

	tr := TranslatedT{
		SrcTokenized: strings.TrimSpace(srctok),
	}
	for _, member := range resp.Params.Param.Value.Struct.Member {
		switch member.Name {
		case "align":
			tr.AlignmentRaw = make([]AlignmentRawT, 0)
			for _, v := range member.Value.Array.Data.Value {
				a := AlignmentRawT{
					SrcStart: -1,
					SrcEnd:   -1,
					TgtStart: -1,
					TgtEnd:   -1,
				}
				for _, member := range v.Struct.Member {
					value := member.Value.I4
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
		case "text":
			tr.TgtTokenized = strings.TrimSpace(member.Value.String)
			if dodetok {
				tr.Text = untok(tr.TgtTokenized, tgtlang)
			}
		}
	}
	repl.Translation[0].Translated[0] = tr

	return repl
}

func decodeMulti(resp *MethodResponseT, srctok string, dodetok bool, tgtlang, id string) *Json {

	repl := &Json{
		Translation:  make([]TranslationT, 1),
		ErrorMessage: "OK",
	}
	repl.Translation[0].Translated = make([]TranslatedT, 0)
	repl.Translation[0].TranslationId = id

	var nbest []ValueT
	for _, member := range resp.Params.Param.Value.Struct.Member {
		if member.Name == "nbest" {
			nbest = member.Value.Array.Data.Value
		}
	}

	for i, translated := range nbest {
		tr := TranslatedT{
			SrcTokenized: strings.TrimSpace(srctok),
			Rank:         i,
		}
		for _, member := range translated.Struct.Member {
			switch member.Name {
			case "align":
				tr.AlignmentRaw = make([]AlignmentRawT, 0)
				for _, v := range member.Value.Array.Data.Value {
					a := AlignmentRawT{
						SrcStart: -1,
						SrcEnd:   -1,
						TgtStart: -1,
						TgtEnd:   -1,
					}
					for _, member := range v.Struct.Member {
						value := member.Value.I4
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
				tr.TgtTokenized = strings.TrimSpace(member.Value.String)
				if dodetok {
					tr.Text = untok(tr.TgtTokenized, tgtlang)
				}
			case "totalScore":
				tr.Score = member.Value.Double
			}
		}
		repl.Translation[0].Translated = append(repl.Translation[0].Translated, tr)
	}

	return repl
}

func first(r *http.Request, opt string) string {
	if len(r.Form[opt]) > 0 {
		return strings.TrimSpace(r.Form[opt][0])
	}
	return ""
}

func rerror(w http.ResponseWriter, code int, msg string) {
	fmt.Fprintf(w, `{
    "errorCode": %d,
    "errorMessage": %q
}
`, code, msg)
}

func quote(s string) string {
	return "'" + strings.Replace(s, "'", "'\\''", -1) + "'"
}

func doCmd(format string, a ...interface{}) string {
	cmd := exec.Command(SH, "-c", fmt.Sprintf(format, a...))
	cmd.Env = []string{
		"ALPINO_HOME=" + ALPINO,
		"PATH=" + PATH,
		"LANG=en_US.utf8",
		"LANGUAGE=en_US.utf8",
		"LC_ALL=en_US.utf8",
	}
	pipe, _ := cmd.StdoutPipe()
	cmd.Start()
	b, _ := ioutil.ReadAll(pipe)
	pipe.Close()
	cmd.Wait()
	return strings.TrimSpace(string(b))
}

func tokNL(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return doCmd("echo %s | $ALPINO_HOME/Tokenization/tokenize_no_breaks.sh | %s --model corpus/truecase-model.nl", quote(s), TRUECASER)
}

func tokEN(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return doCmd("echo %s | %s -l en | %s --model corpus/truecase-model.en", quote(s), TOKENIZER, TRUECASER)
}

func untok(s, lang string) string {
	// er is geen detokenizer voor Nederlands!
	return doCmd("echo %s | %s | %s -l en", quote(s), DETRUECASER, DETOKENIZER)
}

func doMoses(r *Request) ([]byte, error) {
	port := "9071"
	if r.SourceLang == "nl" {
		port = "9072"
	}

	var buf bytes.Buffer

	fmt.Fprintf(&buf, `<?xml version="1.0"?>
<methodCall>
  <methodName>translate</methodName>
  <params>
    <param>
      <value>
        <struct>
          <member>
            <name>text</name>
            <value>%s</value>
          </member>
`, html.EscapeString(r.Tokenized))
	if r.AlignmentInfo {
		fmt.Fprint(&buf,
			`          <member>
            <name>align</name>
            <value><boolean>1</boolean></value>
          </member>
`)
	}
	if r.NBestSize > 1 {
		fmt.Fprintf(&buf,
			`          <member>
            <name>nbest</name>
            <value><i4>%d</i4></value>
          </member>
          <member>
            <name>nbest-distinct</name>
            <value><boolean>1</boolean></value>
          </member>
`, r.NBestSize)
	}
	fmt.Fprint(&buf,
		`        </struct>
      </value>
    </param>
  </params>
</methodCall>
`)

	resp, err := http.Post("http://127.0.0.1:"+port+"/RPC2", "text/xml", &buf)

	if err != nil {
		return []byte{}, err
	}

	b, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	return b, nil
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
        "text":          "Dit is een test.",
        "detokenize":    true,
        "alignmentInfo": true,
        "nBestSize":     10
    }' http://zardoz.service.rug.nl:9070/rpc
</pre>
<p>
or:
<pre>
    <a href="http://zardoz.service.rug.nl:9070/rpc?action=translate&amp;sourceLang=nl&amp;targetLang=en&amp;text=Dit+is+een+test.&amp;detokenize=true&amp;alignmentInfo=true&amp;nBestSize=10">http://zardoz.service.rug.nl:9070/rpc?action=translate&amp;sourceLang=nl&amp;targetLang=en&amp;text=Dit+is+een+test.&amp;detokenize=true&amp;alignmentInfo=true&amp;nBestSize=10</a>
</pre>
<p>
See: <a href="https://github.com/ufal/mtmonkey/blob/master/API.md">API</a>
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
