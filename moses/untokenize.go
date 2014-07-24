package main

/*

  This is just for testing.

*/

//. Imports
//=========

import (
	"github.com/pebbe/util"

	"bufio"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	rePunct = regexp.MustCompile(`^[.!?]+$`)
)

func main() {

	if len(os.Args) != 2 || (os.Args[1] != "en" && os.Args[1] != "nl") || util.IsTerminal(os.Stdin) {

		fmt.Printf(`
Usage: %s language < text

   ... where language is one of: en nl

`, os.Args[0])
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println(untok(scanner.Text(), os.Args[1]))
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}

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
