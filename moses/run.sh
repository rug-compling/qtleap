#!/bin/bash

if [ "`hostname -s`" != "zardoz" ]
then
    echo Dit moet op zardoz
    exit 1
fi


cd `dirname $0`

# dit is 10% van het geheugen (swap niet meegerekend)
ulimit -v 52830340

export LD_LIBRARY_PATH=/net/aps/64/opt/xmlrpc-c/lib

case "`ps -p $(< pid1.txt)`" in
    *mosesserver*)
	;;
    *)
	echo Starting mosesserver en-nl
	/net/aps/64/opt/moses/mosesdecoder/bin/mosesserver -f corpus/moses-en-nl.ini --server-port 9071 >/dev/null 2>/dev/null &
	echo $! > pid1.txt
	;;
esac

case "`ps -p $(< pid2.txt)`" in
    *mosesserver*)
	;;
    *)
	echo Starting mosesserver nl-en
	/net/aps/64/opt/moses/mosesdecoder/bin/mosesserver -f corpus/moses-nl-en.ini --server-port 9072 >/dev/null 2>/dev/null &
	echo $! > pid2.txt
esac

case "`ps -p $(< pid.txt)`" in
    *mserver*)
	;;
    *)
	cat mserver.out
	echo Starting mserver
	./mserver > mserver.out 2>&1 &
	echo $! > pid.txt
	;;
esac
