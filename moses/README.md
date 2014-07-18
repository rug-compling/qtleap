This runs on http://zardoz.service.rug.nl:9070/

See also: [API](https://github.com/ufal/mtmonkey/blob/master/API.md)

It is slow, is this even useful? This is state of the art?

This works on a model that was trained with these escapes:

 - &amp; → &amp;amp;
 - | → &amp;#124;

Problems:

 - There are no rules for detokenizing Dutch. Using the English rules
   will give incorrect results for m-dash. And does it know about
   Dutch quotes?
