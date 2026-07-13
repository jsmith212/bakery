#!/usr/bin/env python3
# Drive the REAL bitbake bb.fetch2 Wget fetcher's checkstatus() against a set of
# URLs handed on the command line.
#
# checkstatus() is pure-python urllib -- it is bitbake's own HTTPMethodFallback
# opener (lib/bb/fetch2/wget.py), NOT the wget binary -- so this phase needs no
# wget at all. The whole point of driving the ACTUAL client is that the decision
# "is this object present?" is made by bitbake's code, exactly as it is at the
# start of a real build's setscene HEAD storm.
#
# The public Fetch.checkstatus() does not return a bool: it returns None when the
# object is present and RAISES bb.fetch2.FetchError when it is missing (only the
# per-fetcher Wget.checkstatus returns True/False). So a hit is the no-exception
# path and a miss is the FetchError path.
#
# One "RESULT <url> <hit|miss>" line is printed per URL. That stdout is the
# corroborating signal; the AUTHORITATIVE proof is the server's own request log,
# which the Go test asserts on (a miss must produce zero GETs).
import os
import sys
import tempfile

import bb
import bb.data
import bb.fetch2

d = bb.data.init()
tmp = tempfile.mkdtemp(prefix="bbconf-")
d.setVar("DL_DIR", os.path.join(tmp, "dl"))
os.makedirs(d.getVar("DL_DIR"), exist_ok=True)
d.setVar("PERSISTENT_DIR", os.path.join(tmp, "persist"))

rc = 0
for url in sys.argv[1:]:
    f = bb.fetch2.Fetch([url], d)
    try:
        f.checkstatus()
        print("RESULT %s hit" % url)
    except bb.fetch2.FetchError:
        print("RESULT %s miss" % url)
    except Exception as e:  # noqa: BLE001 -- any other failure is a driver bug
        print("RESULT %s error:%s:%s" % (url, type(e).__name__, e))
        rc = 2

sys.stdout.flush()
sys.exit(rc)
