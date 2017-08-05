#!/bin/bash

#
# This checks if the mosesservers and mserver are running.
# If not, it starts them.
# If this is called from crontab, messages will be mailed to owner of crontab file
#
# Run this before the first time you call this script:
#    echo 1 > pid.txt
#    echo 1 > pid1.txt
#    echo 1 > pid2.txt
#    touch mserver.out
#

servername=$(< /net/aps/64/servername)

if [ "`hostname -s`" != "$servername" ]
then
    echo This must be run on $servername
    exit 1
fi


cd `dirname $0`

ln -s lock.$$ lock
if [ "`readlink lock`" != lock.$$ ]
then
    echo Getting lock failed
    exit
fi

# This is 10% of memory (not counting swap)
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
	# Errors of previous run
	cat mserver.out

	echo Starting mserver

	# Don't redirect to mserver.log, that file is used by mserver
	./mserver > mserver.out 2>&1 &
	echo $! > pid.txt
	;;
esac

rm -f lock
