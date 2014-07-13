This runs on http://zardoz.service.rug.nl:9070/

See also: [API](https://github.com/ufal/mtmonkey/blob/master/API.md)

It is slow, and with only one sentence per request, is this even useful?
This is state of the art?

Problems:

 - Alpino tokenizer changes square brackets into parentheses.
 - Moses can't handle vertical bar, so it is changed into underscore.
 - The detruecaser won't handle Dutch words like `'s middags` and `ijzer`
   correctly.
 - There are no rules for detokenizing Dutch. Using the English rules
   will give incorrect results for m-dash. And does it know about
   Dutch quotes?
