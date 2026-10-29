uBO scriptlet runtime, vendored from uBlock Origin 1.75.0
==========================================================

Source: https://github.com/gorhill/uBlock/releases/tag/1.75.0
Files:  uBlock0.chromium/js/resources/*.js,
        uBlock0.chromium/js/{arglist-parser,jsonpath,urlskip}.js

License: GPL-3.0 (see uBO-LICENSE.txt). The bundle keeps uBO's header.

Rebuild (Node is only needed for this step; the output is committed):

    npx esbuild entry.js --bundle --format=iife --minify \
        --outfile=../scriptlets.min.js

Then prepend uBO's license header from resources/scriptlets.js to
../scriptlets.min.js.
