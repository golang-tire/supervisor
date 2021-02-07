Supervisor
======
this repo i a renamed fork of [suture](github.com/thejerf/suture) with flowing changes:

    [] - keep version 4 and remove others
    [] - version: supervisor will receive a version to be used and printed in logs
    [] - use zap as logger


How to install
--------------

    import "github.com/golang-tire/supervisor"

Supervisor provides Erlang-ish supervisor trees for Go.

It is intended to deal gracefully with the real failure cases that can
occur with supervision trees (such as burning all your CPU time endlessly
restarting dead services), while also making no unnecessary demands on the
"service" code, and providing hooks to perform adequate logging with in a
production environment.

[A blog post describing the design decisions](http://www.jerf.org/iri/post/2930)
is available.

Versions
--------------

* 0.1.0
  * Initial release.
