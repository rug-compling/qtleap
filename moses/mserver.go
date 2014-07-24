package main

/*

  Moses was trained with these escapes:

    & -> &amp;
    | -> &#124;

*/

//. Imports
//=========

import (
	"github.com/pebbe/tokenize" // For Dutch

	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
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
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

//. Constants
//===========

const (
	// Configuration
	cfgPortEnNl      = "9071"
	cfgPortNlEn      = "9072"
	cfgSH            = "/bin/sh"
	cfgPATH          = "/bin:/usr/bin:/net/aps/64/bin"
	cfgTRUECASEMODEL = "corpus/data/truecase-model"
	cfgTOKENIZER     = "/net/aps/64/opt/moses/mosesdecoder/scripts/tokenizer/tokenizer.perl" // For English
	cfgLOGFILE       = "mserver.log"
	cfgPOOLSIZE      = 12   // Identical to number of threads in each of the two mosesservers
	cfgQUEUESIZE     = 1000 // Including request currently being processed

	// For the logger
	logEXIT = "***EXIT***"
)

//. Global variables
//==================

var (
	rePar      = regexp.MustCompile("\n\\s*\n")
	rePunct    = regexp.MustCompile(`^[.!?]+$`)
	reEndPoint = regexp.MustCompile(`\pL\pL\pP*[.!?]\s*$`)
	reMidPoint = regexp.MustCompile(`\p{Ll}\p{Ll}\pP*[.!?]\s+('s\s+|'t\s+)?\p{Lu}`)
	reLet      = regexp.MustCompile(`\pL`)
	reLetNum   = regexp.MustCompile(`\pL|\pN`)

	// For the truecaser
	best  = make(map[string]map[string]string)
	known = make(map[string]map[string]bool)

	// For the logger
	chLog = make(chan string)

	// For the RPC handler
	chPoolNL = make(chan bool, cfgPOOLSIZE)
	chPoolEN = make(chan bool, cfgPOOLSIZE)
	chQueue  = make(chan bool, cfgQUEUESIZE*2) // Plenty of room for simultaneous requests
)

//. Types for XML-RPC
//===================

type MethodCallT struct {
	XMLName    xml.Name  `xml:"methodCall"`
	MethodName string    `xml:"methodName"`
	Params     []MemberT `xml:"params>param>value>struct>member"`
}

type MethodResponseT struct {
	XMLName xml.Name  `xml:"methodResponse"`
	Params  []MemberT `xml:"params>param>value>struct>member,omitempty"`
	Fault   []MemberT `xml:"fault>value>struct>member,omitempty"`
}

type MemberT struct {
	Name  string `xml:"name"`
	Value ValueT `xml:"value"`
}

type ValueT struct {
	I4      *int      `xml:"i4,omitempty"`
	Int     *int      `xml:"int,omitempty"`
	Boolean int       `xml:"boolean,omitempty"`
	String  string    `xml:"string,omitempty"`
	Text    string    `xml:",chardata"`
	Double  float64   `xml:"double,omitempty"`
	Struct  []MemberT `xml:"struct>member,omitempty"`
	Array   []ValueT  `xml:"array>data>value,omitempty"`
}

// Wrapper to store a response from mosesserver
type MosesT struct {
	mt     *MethodResponseT
	tok    string
	err    string
	errnum int
}

//. Types for output in JSON format
//=================================

type OutputT struct {
	ErrorCode    int            `json:"errorCode"`
	ErrorMessage string         `json:"errorMessage"`
	TimeWait     string         `json:"timeWait"`
	TimeWork     string         `json:"timeWork"`
	Translation  []TranslationT `json:"translation"`
}

type TranslationT struct {
	ErrorCode    int           `json:"errorCode,omitempty"`
	ErrorMessage string        `json:"errorMessage,omitempty"`
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

//. Type for call to mosesserver
//==============================

type MosesParamsT struct {
	// This is in 'json' format, for later conversion to XML-RPC format
	Text          string `json:"text"`
	NBest         int    `json:"nbest"`
	NBestDistinct bool   `json:"nbest-distinct"`
	Align         bool   `json:"align,omitempty"`
}

//. Type for user request
//=======================

type RequestT struct {
	Action        string `json:"action"`
	SourceLang    string `json:"sourceLang"`
	TargetLang    string `json:"targetLang"`
	AlignmentInfo bool   `json:"alignmentInfo"`
	NBestSize     int    `json:"nBestSize"`
	Detokenize    bool   `json:"detokenize"`
	Text          string `json:"text"`
}

//. Main
//======

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	//
	// Start the logger
	//

	var wgLogger sync.WaitGroup
	wgLogger.Add(1)
	go func() {
		logger()
		wgLogger.Done()
	}()

	//
	// Load data for the truecaser
	//

	for _, lang := range []string{"nl", "en"} {
		best[lang] = make(map[string]string)
		known[lang] = make(map[string]bool)
		fp, err := os.Open(cfgTRUECASEMODEL + "." + lang)
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

	//
	// Set-up handlers and start serving
	//

	http.HandleFunc("/", info)
	http.HandleFunc("/rpc", handleJSON)
	http.HandleFunc("/xmlrpc", handleXML)
	http.HandleFunc("/favicon.ico", favicon)
	http.HandleFunc("/robots.txt", robots)

	logf("Server starting")
	logf("Server exit: %v", http.ListenAndServe(":9070", serverLog(http.DefaultServeMux)))
	chLog <- logEXIT
	wgLogger.Wait()

}

//. Handlers for RPC
//==================

func handleJSON(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/rpc" {
		http.NotFound(w, r)
		return
	}
	handle(w, r, false)
}

func handleXML(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/xmlrpc" {
		http.NotFound(w, r)
		return
	}
	handle(w, r, true)
}

func handle(w http.ResponseWriter, r *http.Request, isXmlrpc bool) {

	//
	// Check if we can handle this request
	//

	if len(chQueue) >= cfgQUEUESIZE {
		logf("Too many request")
		http.Error(w, "Too many requests", http.StatusServiceUnavailable)
		return
	}
	start1 := time.Now()
	chQueue <- true
	defer func() { <-chQueue }()

	//
	// Catch loss of connection
	//

	var chClose <-chan bool
	if f, ok := w.(http.CloseNotifier); ok {
		chClose = f.CloseNotify()
	} else {
		chClose = make(<-chan bool)
	}

	//
	// Parse request
	//

	req := &RequestT{
		AlignmentInfo: false,
		NBestSize:     1,
		Detokenize:    true,
	}
	if isXmlrpc {

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "txt/xml")

		b, _ := ioutil.ReadAll(r.Body)
		r.Body.Close()

		call := &MethodCallT{}
		err := xml.Unmarshal(b, call)
		if err != nil {
			responseError(w, isXmlrpc, 5, "Parse error: "+err.Error())
			return
		}

		logf("Method: %v", call.MethodName)
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
			responseError(w, isXmlrpc, 5, "Method \""+call.MethodName+"\" is not supported")
			return
		}

		for _, p := range call.Params {
			switch p.Name {
			case "action":
				req.Action = xmlrpcGetString(p)
			case "sourceLang":
				req.SourceLang = xmlrpcGetString(p)
			case "targetLang":
				req.TargetLang = xmlrpcGetString(p)
			case "alignmentInfo":
				req.AlignmentInfo = xmlrpcGetBool(p)
			case "text":
				req.Text = xmlrpcGetString(p)
			case "nBestSize":
				req.NBestSize = xmlrpcGetInt(p)
			case "detokenize":
				req.Detokenize = xmlrpcGetBool(p)
			}
		}

	} else { // JSON

		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			r.ParseForm()
			req.Action = getGetOption(r, "action")
			req.SourceLang = getGetOption(r, "sourceLang")
			req.TargetLang = getGetOption(r, "targetLang")
			req.Text = getGetOption(r, "text")
			if getGetOption(r, "alignmentInfo") == "true" {
				req.AlignmentInfo = true
			}
			if getGetOption(r, "detokenize") == "false" {
				req.Detokenize = false
			}
			if n, err := strconv.Atoi(getGetOption(r, "nBestSize")); err == nil {
				req.NBestSize = n
			}
		case "POST":
			w.Header().Set("Content-Type", "application/json")
			b, _ := ioutil.ReadAll(r.Body)
			r.Body.Close()
			err := json.Unmarshal(b, req)
			if err != nil {
				responseError(w, isXmlrpc, 5, "Parse error: "+err.Error())
				return
			}
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

	}

	//
	// Check if options are valid, and if so, add request to pool
	//

	if req.Action != "translate" {
		responseError(w, isXmlrpc, 5, "value of 'action' should be 'translate")
		return
	}

	if req.NBestSize < 1 {
		req.NBestSize = 1
	} else if req.NBestSize > 10 {
		req.NBestSize = 10
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
		responseError(w, isXmlrpc, 3, "Invalid combination of SourceLang + TargetLang")
		return
	}

	//
	// Write to pool has completed, so now we can start processing
	//

	start2 := time.Now()
	time1 := start2.Sub(start1)

	//
	// Don't do anything if we lost the connection
	//

	select {
	case <-chClose:
		logf("Request dropped")
		return
	default:
	}

	//
	// Split text into paragraphs, then paragraphs into sentences.
	// Tokenize and truecase sentences, and collect them into 'lines'.
	//

	parts := strings.Split(rePar.ReplaceAllLiteralString(req.Text, "\n\n"), "\n\n")
	lines := make([]string, 0)
	for _, part := range parts {

		part = strings.TrimSpace(part)

		// Does this paragraph contain one sentence per line, or running text?

		isRun := isRunningText(part)
		if isRun {
			// If running text, change newlines to spaces
			part = strings.Replace(part, "\n", " ", -1)
		}

		// Tokenize and split into sentences.

		var ss string
		var err error
		if req.SourceLang == "nl" {
			// Dutch tokenizer also splits input into sentences if last argument is true
			ss, err = tokenize.Dutch(part, isRun)
			if err != nil {
				responseError(w, isXmlrpc, 8, "Tokenizer: "+err.Error())
				return
			}
			ss = strings.TrimSpace(ss)
		} else {
			ss, err = doCmd("echo %s | %s -l en", shellQuote(part), cfgTOKENIZER)
			if err != nil {
				responseError(w, isXmlrpc, 8, "Tokenizer: "+err.Error())
				return
			}
			ss = html.UnescapeString(ss)
			if isRun {
				ss = splitSentences(part, ss)
			}
		}

		// At this point we escape '&' and '|' because those were the escapes in the training data

		ss = escape(ss)

		// Truecase each line, and append to 'lines'

		for _, s := range strings.Split(ss, "\n") {
			s = strings.TrimSpace(s)
			if s != "" {

				// Truecase
				words := strings.Fields(s)
				sentenceStart := true
				for i, word := range words {
					lcword := strings.ToLower(word)
					if w, ok := best[req.SourceLang][lcword]; sentenceStart && ok {
						// Truecase sentence start
						words[i] = w
					} else if known[req.SourceLang][word] {
						// Don't change known words
					} else if w, ok := best[req.SourceLang][lcword]; ok {
						// Truecase otherwise unknown words
						words[i] = w
					}
					switch sentenceStart {
					case false:
						if word == ":" || rePunct.MatchString(word) {
							sentenceStart = true
						}
					case true:
						if reLetNum.MatchString(word) && word != "'s" && word != "'t" {
							sentenceStart = false
						}
					}
				}
				s = strings.Join(words, " ")

				lines = append(lines, s)
			}
		}
	}

	//
	// Send each tokenized and truecased sentence to mosesserver, and collect the results in 'responses'
	//

	responses := make([]*MosesT, len(lines))
	for idx, line := range lines {
		rs := &MosesT{
			mt:  &MethodResponseT{},
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
				logf("Moses: %v", err)
				rs.err = err.Error()
				rs.errnum = 8
			} else {
				if err := xml.Unmarshal(resp, rs.mt); err != nil {
					panic(err)
				}
				if len(rs.mt.Fault) > 0 {
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
		responses[idx] = rs
	}

	//
	// Convert list of moses responses, and store into 'reply'
	//

	reply := &OutputT{
		Translation:  make([]TranslationT, len(responses)),
		ErrorCode:    0,
		ErrorMessage: "OK",
	}
	for idx, resp := range responses {

		if resp.errnum != 0 || resp.err != "" {
			reply.ErrorCode = 99
			reply.ErrorMessage = "Failed to translate some sentence(s)"
			reply.Translation[idx].ErrorCode = resp.errnum
			reply.Translation[idx].ErrorMessage = resp.err
			if resp.errnum == 0 {
				reply.Translation[idx].ErrorCode = 99
			}
			continue
		}

		reply.Translation[idx].Translated = make([]TranslatedT, 0)

		if req.AlignmentInfo || len(responses) > 1 {
			reply.Translation[idx].SrcTokenized = strings.TrimSpace(unescape(resp.tok))
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
					if req.Detokenize {
						tr.Text = untok(untrue(tr.Tokenized, req.TargetLang), req.TargetLang)
					} else {
						tr.Text = tr.Tokenized
					}
				case "totalScore":
					tr.Score = member.Value.Double
				}
			}
			if !req.AlignmentInfo {
				tr.Tokenized = ""
			}
			reply.Translation[idx].Translated = append(reply.Translation[idx].Translated, tr)
		}
	}

	//
	// Calculate the alignment values for tgt-end
	//

	if req.AlignmentInfo {
		for _, t := range reply.Translation {
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

	//
	// Done processing
	//

	time2 := time.Now().Sub(start2)
	reply.TimeWait = time1.String()
	reply.TimeWork = time2.String()

	logf("Requests: %d - Wait: %v - Work: %v - Lines: %d", len(chQueue), time1, time2, len(lines))

	//
	// Send result
	//

	if isXmlrpc {
		fmt.Fprintln(w, "<?xml version='1.0' encoding='UTF-8'?>\n<methodResponse>")
		xmlRPCParamsMarshal(*reply, w)
		fmt.Fprintln(w, "</methodResponse>")
	} else { // JSON
		b, _ := json.MarshalIndent(reply, "", "  ")
		fmt.Fprintln(w, string(b))
	}
}

//. Analysing and converting text
//===============================

func splitSentences(ori, tok string) string {
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

func isRunningText(part string) bool {

	lines := strings.Split(part, "\n")

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

func untrue(s, lang string) string {
	// Input was a single sentence, but it could be translated into more than one sentence
	inwords := strings.Fields(s)
	outwords := make([]string, len(inwords))
	state := 1 // Need cap
	for i, word := range inwords {
		switch state {
		case 0: // Normal
			outwords[i] = word
			if rePunct.MatchString(word) {
				state = 1
			} else if word == ":" && i < len(inwords)-1 && strings.Contains(`"”„“`, inwords[i+1]) {
				state = 1
			}
		case 1: // Need cap
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
			for j, c := range word { // Use range in case first char is multibyte
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

	words := strings.Fields(s)
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

func escape(s string) string {
	return strings.Replace(strings.Replace(s, "&", "&amp;", -1), "|", "&#124;", -1)
}

func unescape(s string) string {
	return strings.Replace(strings.Replace(s, "&#124;", "|", -1), "&amp;", "&", -1)
}

//. Calling the mosesserver
//=========================

func doMoses(sourceLang, tokenized string, alignmentInfo bool, nBestSize int) ([]byte, error) {
	port := cfgPortEnNl
	if sourceLang == "nl" {
		port = cfgPortNlEn
	}

	var buf bytes.Buffer
	js := MosesParamsT{
		Text:          tokenized,
		NBest:         nBestSize,
		NBestDistinct: true,
		Align:         alignmentInfo,
	}
	buf.WriteString(`<?xml version='1.0' encoding='UTF-8'?>
<methodCall>
  <methodName>translate</methodName>
`)
	xmlRPCParamsMarshal(js, &buf)
	buf.WriteString("</methodCall>\n")

	resp, err := http.Post("http://127.0.0.1:"+port+"/RPC2", "text/xml", &buf)

	if err != nil {
		return []byte{}, err
	}

	b, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	return b, nil
}

//. Shell commands
//================

func shellQuote(s string) string {
	return "'" + strings.Replace(s, "'", "'\\''", -1) + "'"
}

func doCmd(format string, a ...interface{}) (string, error) {
	cmd := exec.Command(cfgSH, "-c", fmt.Sprintf(format, a...))
	cmd.Env = []string{
		"PATH=" + cfgPATH,
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

//. Converting JSON to XML-RPC
//============================

func xmlRPCParamsMarshal(i interface{}, w io.Writer) {
	r := reflect.ValueOf(i)
	if r.Kind() == reflect.Ptr {
		panic("Can't call xmlRPCParamsMarshal with pointer variable")
	}

	fmt.Fprint(w, `  <params>
    <param>
      <value>
`)

	xmlrpcMarshal(r, "        ", w)

	fmt.Fprint(w, `      </value>
    </param>
  </params>
`)

}

func xmlrpcMarshal(r reflect.Value, indent string, w io.Writer) {
	switch k := r.Kind(); k {
	case reflect.Struct:
		fmt.Fprintln(w, indent+"<struct>")
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
			if omitempty && xmlrpcIsempty(r2) {
				continue
			}
			fmt.Fprintln(w, indent+"  <member>")
			fmt.Fprintf(w, indent+"    <name>%s</name>\n", html.EscapeString(n))
			fmt.Fprintln(w, indent+"    <value>")
			xmlrpcMarshal(r2, indent+"      ", w)
			fmt.Fprintln(w, indent+"    </value>")
			fmt.Fprintln(w, indent+"  </member>")
		}
		fmt.Fprintln(w, indent+"</struct>")
	case reflect.Bool:
		v := 0
		if r.Bool() {
			v = 1
		}
		fmt.Fprintf(w, indent+"<boolean>%d</boolean>\n", v)
	case reflect.Int:
		fmt.Fprintf(w, indent+"<int>%d</int>\n", r.Int())
	case reflect.Float64:
		fmt.Fprintf(w, indent+"<double>%g</double>\n", r.Float())
	case reflect.String:
		fmt.Fprintf(w, indent+"<string>%s</string>\n", r.String())
	case reflect.Slice:
		fmt.Fprintln(w, indent+"<array>")
		fmt.Fprintln(w, indent+"  <data>")
		for i := 0; i < r.Len(); i++ {
			fmt.Fprintln(w, indent+"    <value>")
			xmlrpcMarshal(r.Index(i), indent+"      ", w)
			fmt.Fprintln(w, indent+"    </value>")
		}
		fmt.Fprintln(w, indent+"  </data>")
		fmt.Fprintln(w, indent+"</array>")
	default:
		panic(fmt.Errorf("unknown type: %s", k))
	}
}

func xmlrpcIsempty(r reflect.Value) bool {
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
			if !xmlrpcIsempty(r.Field(i)) {
				return false
			}
		}
		return true
	case reflect.Bool:
		return !r.Bool()
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
	case reflect.Slice:
		for i := 0; i < r.Len(); i++ {
			if !xmlrpcIsempty(r.Index(i)) {
				return false
			}
		}
		return true
	default:
		panic(fmt.Errorf("unknown type: %s", k))
	}
	return false
}

//. Parsing XML-RPC
//=================

func xmlrpcGetString(p MemberT) string {
	if p.Value.String != "" {
		return p.Value.String
	}
	return p.Value.Text
}

func xmlrpcGetBool(p MemberT) bool {
	return p.Value.Boolean != 0
}

func xmlrpcGetInt(p MemberT) int {
	if p.Value.Int != nil {
		return *p.Value.Int
	}
	if p.Value.I4 != nil {
		return *p.Value.I4
	}
	return 0
}

//. Helpers for HTTP handlers
//===========================

func serverLog(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logf("[%s] %s %s %s", r.Header.Get("X-Forwarded-For"), r.RemoteAddr, r.Method, r.URL.Path)
		handler.ServeHTTP(w, r)
	})
}

func getGetOption(r *http.Request, opt string) string {
	if len(r.Form[opt]) > 0 {
		return strings.TrimSpace(r.Form[opt][0])
	}
	return ""
}

func responseError(w http.ResponseWriter, isXmlrpc bool, code int, msg string) {
	if isXmlrpc {
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

//. HTTP handlers for static pages
//================================

func init() {
	var b []byte
	b, _ = base64.StdEncoding.DecodeString(file__favicon__ico)
	file__favicon__ico = string(b)
}

func cache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
}

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

func favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	cache(w)
	fmt.Fprint(w, file__favicon__ico)
}

func robots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	cache(w)
	fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
}

func info(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
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

//. Logger
//========

func logf(format string, v ...interface{}) {
	chLog <- fmt.Sprintf(format, v...)
}

func logger() {

	rotate := func() {
		for i := 4; i > 1; i-- {
			os.Rename(
				fmt.Sprintf("%s%d", cfgLOGFILE, i-1),
				fmt.Sprintf("%s%d", cfgLOGFILE, i))
		}
		os.Rename(cfgLOGFILE, cfgLOGFILE+"1")
	}

	rotate()
	fp, err := os.Create(cfgLOGFILE)
	if err != nil {
		log.Fatal(err)
	}

	n := 0
	for {
		msg := <-chLog
		if msg == logEXIT {
			fp.Close()
			return
		}
		now := time.Now()
		s := fmt.Sprintf("%04d-%02d-%02d %d:%02d:%02d %s",
			now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(),
			msg)
		fmt.Fprintln(fp, s)
		fp.Sync()
		n++
		if n == 10000 {
			fp.Close()
			rotate()
			fp, _ = os.Create(cfgLOGFILE)
			n = 0
		}
	}
}
