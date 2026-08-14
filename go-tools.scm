;; go-tools — Go development tools for Helix, backed by the hx-go-tool sidecar.
;;
;; Commands (bind them or run from the : prompt):
;;   :go-test            run the test under the cursor (or the package tests)
;;   :go-test-package    run the current file's package tests
;;   :go-test-all        run all tests in the module
;;   :go-test-rerun      repeat the last test run
;;   :go-test-panel      focus/toggle the test results panel
;;   :go-coverage        run package tests with coverage, mark uncovered lines
;;   :go-coverage-clear  remove coverage marks
;;   :go-tags-add        add struct field tags   (:go-tags-add json,yaml [omitempty])
;;   :go-tags-remove     remove struct field tags
;;   :go-tags-clear      clear all tags on the struct under the cursor
;;   :go-impl            insert interface method stubs (:go-impl s *Server io.Reader)
;;   :go-gotests         generate a table-driven test for the function under the cursor
;;   :go-mod-panel       open the go.mod dependency panel
;;
;; The sidecar binary is looked up in <cog>/bin/hx-go-tool, then on PATH.

(require "helix/components.scm")
(require "helix/misc.scm")
(require "helix/editor.scm")
(require "helix/ext.scm")
(require (prefix-in helix. "helix/commands.scm"))
(require (prefix-in static. "helix/static.scm"))
(require-builtin steel/process)
(require-builtin steel/json)
(require-builtin helix/core/text as text.)

;; ===== Config =====

(define *gt-sidecar-override* #f)
(define *gt-panel-height*     12)   ; content rows of the test panel
(define *gt-test-race*        #f)
(define *gt-test-verbose*     #f)
(define *gt-test-timeout*     "")
(define *gt-tags-transform*   "snakecase")

(provide go-tools-set-sidecar!
         go-tools-set-panel-height!
         go-tools-set-race!
         go-tools-set-verbose!
         go-tools-set-timeout!
         go-tools-set-tags-transform!)

;;@doc
;; Point the plugin at a specific hx-go-tool binary.
(define (go-tools-set-sidecar! path) (set! *gt-sidecar-override* path))

;;@doc
;; Content height of the test results panel (rows).
(define (go-tools-set-panel-height! h) (set! *gt-panel-height* (max 4 (min 40 h))))

;;@doc
;; Run tests with -race.
(define (go-tools-set-race! enabled?) (set! *gt-test-race* enabled?))

;;@doc
;; Capture output of passing tests too (go test -v).
(define (go-tools-set-verbose! enabled?) (set! *gt-test-verbose* enabled?))

;;@doc
;; go test -timeout value, e.g. "60s". Empty string uses the default.
(define (go-tools-set-timeout! t) (set! *gt-test-timeout* t))

;;@doc
;; Tag value casing for :go-tags-add — snakecase | camelcase | lispcase | pascalcase | keep.
(define (go-tools-set-tags-transform! t) (set! *gt-tags-transform* t))

;; ===== Small utilities =====

(define (gt-take lst n)
  (if (or (null? lst) (<= n 0)) '() (cons (car lst) (gt-take (cdr lst) (- n 1)))))

(define (gt-drop lst n)
  (if (or (null? lst) (<= n 0)) lst (gt-drop (cdr lst) (- n 1))))

(define (gt-repeat-str s n)
  (if (<= n 0) "" (string-append s (gt-repeat-str s (- n 1)))))

(define (gt-truncate s max-w)
  (if (<= (string-length s) max-w)
      s
      (if (<= max-w 1) "…" (string-append (substring s 0 (- max-w 1)) "…"))))

(define (gt-pad-right s width)
  (if (>= (string-length s) width)
      s
      (string-append s (gt-repeat-str " " (- width (string-length s))))))

(define (gt-div2 n) (quotient n 2))

(define (gt-last-slash-index s)
  (let loop ([i (- (string-length s) 1)])
    (cond [(< i 0) #f]
          [(equal? (string-ref s i) #\/) i]
          [else (loop (- i 1))])))

(define (gt-parent-path path)
  (define idx (gt-last-slash-index path))
  (cond [(not idx) path]
        [(= idx 0) "/"]
        [else (substring path 0 idx)]))

;; Nearest ancestor of dir containing go.mod, or #f.
(define (gt-module-root dir)
  (let loop ([d dir])
    (cond [(is-file? (string-append d "/go.mod")) d]
          [else (let ([p (gt-parent-path d)])
                  (if (or (equal? p d) (equal? p "/")) #f (loop p)))])))

;; JSON numbers arrive as floats — coerce to an int, tolerating #f.
(define (gt-int x) (if (number? x) (exact (round x)) 0))

;; JSON null arrives as void — treat it like a missing key.
(define (gt-get h key default)
  (let ([v (and (hash? h) (hash-try-get h key))])
    (if (or (not v) (void? v)) default v)))

;; "57.1%" from 57.142857
(define (gt-pct-str p)
  (let* ([tenths (exact (round (* p 10)))]
         [ip (quotient tenths 10)]
         [fp (abs (remainder tenths 10))])
    (string-append (to-string ip) "." (to-string fp) "%")))

;; "0.42s" from 0.421
(define (gt-secs-str s)
  (let* ([hund (exact (round (* s 100)))]
         [ip (quotient hund 100)]
         [fp (abs (remainder hund 100))])
    (string-append (to-string ip) "." (if (< fp 10) "0" "") (to-string fp) "s")))

(define (gt-theme scope fallback)
  (with-handler (lambda (err) (theme-scope-ref fallback)) (theme-scope scope)))

;; ===== Editor helpers =====

(define (gt-current-path)
  (editor-document->path (editor->doc-id (editor-focus))))

(define (gt-go-file? path)
  (and (string? path) (ends-with? path ".go")))

;; get-current-line-number is 0-based; the sidecar and :goto are 1-based.
(define (gt-cursor-line)
  (+ 1 (static.get-current-line-number)))

;; ===== Sidecar invocation =====

(define (gt-sidecar-path)
  (define steel-home
    (let ([sh (maybe-get-env-var "STEEL_HOME")])
      (if (string? sh) sh (string-append (or (maybe-get-env-var "HOME") "~") "/.steel"))))
  (define bundled (string-append steel-home "/cogs/helix-golang-plugin/bin/hx-go-tool"))
  (cond [*gt-sidecar-override* *gt-sidecar-override*]
        [(is-file? bundled) bundled]
        [else "hx-go-tool"]))

(define (maybe-get-env-var name)
  (with-handler (lambda (err) #f) (env-var name)))

;; Blocking call — only run on a native thread. Returns a hash; failures
;; come back as (hash 'error msg).
(define (gt-sidecar-exec args)
  (with-handler
   (lambda (err) (hash 'error (to-string err)))
   (let* ([cmd  (command (gt-sidecar-path) args)]
          [_    (set-piped-stdout! cmd)]
          [proc (Ok->value (spawn-process cmd))]
          [out  (Ok->value (wait->stdout proc))])
     (let ([res (string->jsexpr out)])
       (if (hash? res) res (hash 'error (string-append "bad sidecar output: " (to-string out))))))))

;; Run the sidecar off-thread; call on-ok (or on-err with a message) on the
;; main thread. on-err is optional.
(define (gt-call-async args on-ok . maybe-on-err)
  (define on-err
    (if (null? maybe-on-err)
        (lambda (msg) (set-error! (string-append "go: " msg)))
        (car maybe-on-err)))
  (spawn-native-thread
   (lambda ()
     (let ([res (gt-sidecar-exec args)])
       (hx.with-context
        (lambda ()
          (let ([err (hash-try-get res 'error)])
            (if err (on-err err) (on-ok res)))
          (helix.redraw)))))))

(define (gt-test-flags)
  (append (if *gt-test-race* (list "-race") '())
          (if *gt-test-verbose* (list "-verbose") '())
          (if (> (string-length *gt-test-timeout*) 0)
              (list "-timeout" *gt-test-timeout*)
              '())))

;; ===== Generic help overlay =====

(define *gt-help-title* "")
(define *gt-help-lines* '())

(struct GtHelpState ())

(define (gt-help-render state rect frame)
  (define W (area-width rect))
  (define H (area-height rect))
  (define bw (min 46 (- W 4)))
  (define bh (min (+ (length *gt-help-lines*) 2) (- H 2)))
  (define x0 (gt-div2 (- W bw)))
  (define y0 (gt-div2 (- H bh)))
  (define bg  (theme-scope-ref "ui.background"))
  (define brd (theme-scope-ref "ui.window"))
  (define txt (theme-scope-ref "ui.text"))
  (define ttl (theme-scope-ref "ui.statusline.normal"))
  (define panel-area (area x0 y0 bw bh))
  (buffer/clear-with frame panel-area bg)
  (block/render frame panel-area (make-block bg brd "all" "rounded"))
  (frame-set-string! frame (+ x0 2) y0 (string-append "  " *gt-help-title* "  ") ttl)
  (let loop ([lines *gt-help-lines*] [row (+ y0 1)])
    (unless (or (null? lines) (>= row (- (+ y0 bh) 1)))
      (frame-set-string! frame (+ x0 2) row (gt-truncate (car lines) (- bw 4)) txt)
      (loop (cdr lines) (+ row 1)))))

(define (gt-help-handle-event state event)
  (if (key-event? event) event-result/close event-result/ignore))

(define (gt-show-help! title lines)
  (set! *gt-help-title* title)
  (set! *gt-help-lines* lines)
  (enqueue-thread-local-callback
   (lambda ()
     (push-component!
      (new-component! "gt-help" (GtHelpState) gt-help-render
                      (hash "handle_event" gt-help-handle-event))))))

;; ===== Test results panel =====

(define *gtp-active*         #f)
(define *gtp-focused*        #f)
(define *gtp-cursor*         0)
(define *gtp-window-start*   0)
(define *gtp-visible-height* 10)
(define *gtp-y0*             0)     ; panel top row, set during render
(define *gtp-result*         #f)
(define *gtp-rows*           '())
(define *gtp-expanded*       (hash))   ; row key -> #t when expanded
(define *gtp-status*         "no tests run yet — :go-test")
(define *gtp-running*        #f)
(define *gtp-pending-key*    #f)
(define *gt-last-run*        #f)    ; (desc . args)

;; Rows are (kind text file line key status) — kind is 'pkg | 'test | 'out.
(define (gt-row-kind   r) (list-ref r 0))
(define (gt-row-text   r) (list-ref r 1))
(define (gt-row-file   r) (list-ref r 2))
(define (gt-row-line   r) (list-ref r 3))
(define (gt-row-key    r) (list-ref r 4))
(define (gt-row-status r) (list-ref r 5))

(define (gtp-expanded? key)
  (and (hash-try-get *gtp-expanded* key) #t))

(define (gt-status-icon status)
  (cond [(equal? status "pass") "✓"]
        [(equal? status "fail") "✗"]
        [(equal? status "skip") "∅"]
        [(equal? status "build-fail") "⚠"]
        [else "·"]))

(define (gtp-out-rows text indent file line key)
  (map (lambda (l) (list 'out (string-append indent l) file line key ""))
       (filter (lambda (l) (> (string-length l) 0))
               (split-many text "\n"))))

(define (gtp-rebuild-rows!)
  (define rows '())
  (define (add! r) (set! rows (cons r rows)))
  (define res *gtp-result*)
  (cond
    [*gtp-running*
     (add! (list 'out "  running…" #f #f "" ""))]
    [(not (hash? res))
     (add! (list 'out "  no test results yet — run :go-test" #f #f "" ""))]
    [else
     (let ([pkgs  (gt-get res 'packages '())]
           [tests (gt-get res 'tests '())]
           [build (gt-get res 'build_output "")])
       (for-each
        (lambda (p)
          (let* ([pname   (gt-get p 'package "?")]
                 [pstatus (gt-get p 'status "")]
                 [pkey    (string-append "pkg|" pname)]
                 [header  (string-append
                           " " (gt-status-icon pstatus) " " pname
                           "  ✓" (to-string (gt-int (gt-get p 'pass 0)))
                           " ✗" (to-string (gt-int (gt-get p 'fail 0)))
                           " ∅" (to-string (gt-int (gt-get p 'skip 0)))
                           "  (" (gt-secs-str (gt-get p 'elapsed 0)) ")")])
            (add! (list 'pkg header #f #f pkey pstatus))
            (when (and (gtp-expanded? pkey)
                       (> (string-length (gt-get p 'output "")) 0))
              (for-each add! (gtp-out-rows (gt-get p 'output "") "      " #f #f pkey)))
            (for-each
             (lambda (t)
               (when (equal? (gt-get t 'package "") pname)
                 (let* ([tname (gt-get t 'name "?")]
                        [tstatus (gt-get t 'status "")]
                        [tkey  (string-append pname "|" tname)]
                        [file  (let ([f (hash-try-get t 'fail_file)]) (and (string? f) f))]
                        [line  (let ([l (hash-try-get t 'fail_line)]) (and (number? l) (gt-int l)))])
                   (add! (list 'test
                               (string-append "   " (gt-status-icon tstatus) " " tname
                                              "  (" (gt-secs-str (gt-get t 'elapsed 0)) ")")
                               file line tkey tstatus))
                   (when (and (gtp-expanded? tkey)
                              (> (string-length (gt-get t 'output "")) 0))
                     (for-each add! (gtp-out-rows (gt-get t 'output "") "      " file line tkey))))))
             tests)))
        pkgs)
       (when (> (string-length build) 0)
         (add! (list 'pkg " ⚠ build output" #f #f "build" "build-fail"))
         (when (gtp-expanded? "build")
           (for-each add! (gtp-out-rows build "      " #f #f "build")))))])
  (set! *gtp-rows* (reverse rows))
  (let ([n (length *gtp-rows*)])
    (set! *gtp-cursor* (min *gtp-cursor* (max 0 (- n 1))))
    (when (< *gtp-cursor* *gtp-window-start*)
      (set! *gtp-window-start* *gtp-cursor*))))

(define (gtp-auto-expand-failures!)
  (set! *gtp-expanded* (hash))
  (define res *gtp-result*)
  (when (hash? res)
    (for-each
     (lambda (t)
       (when (equal? (gt-get t 'status "") "fail")
         (set! *gtp-expanded*
               (hash-insert *gtp-expanded*
                            (string-append (gt-get t 'package "") "|" (gt-get t 'name ""))
                            #t))))
     (gt-get res 'tests '()))
    (for-each
     (lambda (p)
       (when (equal? (gt-get p 'status "") "build-fail")
         (set! *gtp-expanded*
               (hash-insert *gtp-expanded* (string-append "pkg|" (gt-get p 'package "")) #t))))
     (gt-get res 'packages '()))
    (when (> (string-length (gt-get res 'build_output "")) 0)
      (set! *gtp-expanded* (hash-insert *gtp-expanded* "build" #t)))))

(define (gt-summarize res desc)
  (string-append
   "✓ " (to-string (gt-int (gt-get res 'pass 0)))
   "  ✗ " (to-string (gt-int (gt-get res 'fail 0)))
   "  ∅ " (to-string (gt-int (gt-get res 'skip 0)))
   (let ([cover (hash-try-get res 'cover)])
     (if (hash? cover)
         (string-append "  cover " (gt-pct-str (gt-get cover 'total_pct 0)))
         ""))
   "  — " desc))

;; --- Rendering ---

(struct GtPanelBgState ())

(define (gtp-panel-height total-h)
  (min (+ *gt-panel-height* 2) (max 5 (- total-h 4))))

(define (gtp-render-bg state rect frame)
  (when *gtp-active*
    (define W (area-width rect))
    (define H (area-height rect))
    (define ph (gtp-panel-height H))
    (define y0 (- H ph))
    (set! *gtp-y0* y0)
    (set! *gtp-visible-height* (max 1 (- ph 2)))
    (set-editor-clip-bottom! ph)

    (define bg     (theme-scope-ref "ui.background"))
    (define border (if *gtp-focused*
                       (theme-scope-ref "ui.window")
                       (theme-scope-ref "ui.text")))
    (define txt    (theme-scope-ref "ui.text"))
    (define ttl    (theme-scope-ref "ui.statusline.normal"))
    (define hl     (theme-scope-ref "ui.menu.selected"))
    (define ok-st  (gt-theme "diff.plus" "ui.text"))
    (define bad-st (gt-theme "diff.minus" "ui.text"))
    (define dim-st (gt-theme "comment" "ui.text"))

    (define panel-area (area 0 y0 W ph))
    (buffer/clear-with frame panel-area bg)
    (block/render frame panel-area (make-block bg border "all" "rounded"))
    (frame-set-string! frame 2 y0
                       (gt-truncate (string-append "  Go Tests — " *gtp-status* "  ") (- W 4))
                       ttl)

    (define (row-style row selected?)
      (cond [selected? hl]
            [(equal? (gt-row-kind row) 'out) dim-st]
            [else
             (let ([status (gt-row-status row)])
               (cond [(equal? status "pass") ok-st]
                     [(equal? status "fail") bad-st]
                     [(equal? status "build-fail") bad-st]
                     [else txt]))]))

    (define visible (gt-take (gt-drop *gtp-rows* *gtp-window-start*) *gtp-visible-height*))
    (let loop ([items visible] [row 0])
      (unless (or (null? items) (>= row *gtp-visible-height*))
        (let* ([entry (car items)]
               [abs-idx (+ *gtp-window-start* row)]
               [y (+ y0 1 row)]
               [selected? (and *gtp-focused* (= abs-idx *gtp-cursor*))])
          (when selected?
            (frame-set-string! frame 1 y (make-string (- W 2) #\space) hl))
          (frame-set-string! frame 1 y
                             (gt-truncate (gt-row-text entry) (- W 3))
                             (row-style entry selected?)))
        (loop (cdr items) (+ row 1))))))

(define (gtp-handle-event-bg state event)
  event-result/ignore)

;; --- Navigation / actions ---

(define (gtp-current-row)
  (and (not (null? *gtp-rows*))
       (< *gtp-cursor* (length *gtp-rows*))
       (list-ref *gtp-rows* *gtp-cursor*)))

(define (gtp-cursor-down!)
  (let ([n (length *gtp-rows*)])
    (when (< *gtp-cursor* (- n 1))
      (set! *gtp-cursor* (+ *gtp-cursor* 1))
      (when (> *gtp-cursor* (+ *gtp-window-start* (- *gtp-visible-height* 1)))
        (set! *gtp-window-start* (+ *gtp-window-start* 1))))))

(define (gtp-cursor-up!)
  (when (> *gtp-cursor* 0)
    (set! *gtp-cursor* (- *gtp-cursor* 1))
    (when (< *gtp-cursor* *gtp-window-start*)
      (set! *gtp-window-start* *gtp-cursor*))))

(define (gtp-goto-top!)
  (set! *gtp-cursor* 0)
  (set! *gtp-window-start* 0))

(define (gtp-goto-bottom!)
  (let ([n (length *gtp-rows*)])
    (when (> n 0)
      (set! *gtp-cursor* (- n 1))
      (set! *gtp-window-start* (max 0 (- n *gtp-visible-height*))))))

(define (gtp-half-page! direction)
  (let ([steps (max 1 (gt-div2 *gtp-visible-height*))])
    (let loop ([i 0])
      (when (< i steps)
        (if (equal? direction 'down) (gtp-cursor-down!) (gtp-cursor-up!))
        (loop (+ i 1))))))

(define (gtp-toggle-expand!)
  (define row (gtp-current-row))
  (when row
    (let ([key (gt-row-key row)])
      (when (and (string? key) (> (string-length key) 0))
        (set! *gtp-expanded*
              (hash-insert *gtp-expanded* key (not (gtp-expanded? key))))
        (gtp-rebuild-rows!))))
  event-result/consume)

(define (gtp-activate!)
  (define row (gtp-current-row))
  (cond
    [(not row) event-result/consume]
    [(and (gt-row-file row) (string? (gt-row-file row)))
     (let ([file (gt-row-file row)]
           [line (gt-row-line row)])
       (set! *gtp-focused* #f)
       (enqueue-thread-local-callback
        (lambda ()
          (helix.open file)
          (when (and line (> line 0))
            (helix.goto (to-string line)))))
       event-result/close)]
    [else (gtp-toggle-expand!)]))

(define (gtp-close-all!)
  (set! *gtp-active* #f)
  (set! *gtp-focused* #f)
  (pop-last-component-by-name! "go-test-bg")
  (enqueue-thread-local-callback (lambda () (set-editor-clip-bottom! 0))))

(define (gtp-unfocus!)
  (set! *gtp-focused* #f))

;; --- Keymap ---

(define *gtp-keymap*
  (list
   (list (list #\j 'down)  "j/↓"    "down"             (lambda () (gtp-cursor-down!) event-result/consume))
   (list (list #\k 'up)    "k/↑"    "up"               (lambda () (gtp-cursor-up!)   event-result/consume))
   (list (list #\g)        "gg"     "go to top"        (lambda ()
                                                         (if (equal? *gtp-pending-key* #\g)
                                                             (begin
                                                               (set! *gtp-pending-key* #f)
                                                               (gtp-goto-top!))
                                                             (set! *gtp-pending-key* #\g))
                                                         event-result/consume))
   (list (list #\G)        "G"      "go to bottom"     (lambda () (gtp-goto-bottom!) event-result/consume))
   (list (list 'ctrl-d)    "ctrl-d" "half page down"   (lambda () (gtp-half-page! 'down) event-result/consume))
   (list (list 'ctrl-u)    "ctrl-u" "half page up"     (lambda () (gtp-half-page! 'up)   event-result/consume))
   (list (list #\o 'enter) "o/↵"    "jump / toggle"    (lambda () (gtp-activate!)))
   (list (list 'tab)       "tab"    "expand/collapse"  (lambda () (gtp-toggle-expand!)))
   (list (list #\R)        "R"      "re-run last"      (lambda ()
                                                         (go-test-rerun)
                                                         event-result/consume))
   (list (list #\?)        "?"      "help"             (lambda ()
                                                         (gt-show-help! "Test Panel" (gtp-help-lines))
                                                         event-result/consume))
   (list (list 'escape)    "esc"    "unfocus"          (lambda () (gtp-unfocus!) event-result/close))
   (list (list #\q)        "q"      "close panel"      (lambda () (gtp-close-all!) event-result/close))))

(define (gtp-help-lines)
  (map (lambda (entry)
         (string-append (gt-pad-right (list-ref entry 1) 9) (list-ref entry 2)))
       *gtp-keymap*))

(define (gt-matcher-hits? matcher event ch ctrl?)
  (cond
    [(char? matcher)          (and (char? ch) (not ctrl?) (equal? ch matcher))]
    [(equal? matcher 'enter)  (key-event-enter? event)]
    [(equal? matcher 'tab)    (key-event-tab? event)]
    [(equal? matcher 'escape) (key-event-escape? event)]
    [(equal? matcher 'up)     (key-event-up? event)]
    [(equal? matcher 'down)   (key-event-down? event)]
    [(equal? matcher 'ctrl-d) (and ctrl? (char? ch) (equal? ch #\d))]
    [(equal? matcher 'ctrl-u) (and ctrl? (char? ch) (equal? ch #\u))]
    [else #f]))

(define (gt-any-matcher-hits? matchers event ch ctrl?)
  (cond [(null? matchers) #f]
        [(gt-matcher-hits? (car matchers) event ch ctrl?) #t]
        [else (gt-any-matcher-hits? (cdr matchers) event ch ctrl?)]))

(struct GtPanelFgState ())

(define (gtp-render-fg state rect frame) void)

(define (gtp-cursor-fn-fg state area)
  (position (+ *gtp-y0* 1 (- *gtp-cursor* *gtp-window-start*)) 1))

(define (gtp-handle-event-fg state event)
  (define ch    (key-event-char event))
  (define ctrl? (equal? (key-event-modifier event) key-modifier-ctrl))
  (unless (and (char? ch) (equal? ch #\g))
    (set! *gtp-pending-key* #f))
  (let loop ([entries *gtp-keymap*])
    (cond
      [(null? entries) event-result/consume]
      [(gt-any-matcher-hits? (list-ref (car entries) 0) event ch ctrl?)
       ((list-ref (car entries) 3))]
      [else (loop (cdr entries))])))

(define (gtp-make-bg-component)
  (new-component! "go-test-bg" (GtPanelBgState) gtp-render-bg
                  (hash "handle_event" gtp-handle-event-bg)))

(define (gtp-make-fg-component)
  (new-component! "go-test-fg" (GtPanelFgState) gtp-render-fg
                  (hash "handle_event" gtp-handle-event-fg
                        "cursor"       gtp-cursor-fn-fg)))

(define (gtp-ensure-open!)
  (unless *gtp-active*
    (set! *gtp-active* #t)
    (enqueue-thread-local-callback
     (lambda () (push-component! (gtp-make-bg-component))))))

;; ===== Test commands =====

(define (gt-run-tests! desc args)
  (set! *gt-last-run* (cons desc args))
  (set! *gtp-running* #t)
  (set! *gtp-status* (string-append "running " desc " …"))
  (gtp-rebuild-rows!)
  (gtp-ensure-open!)
  (set-status! (string-append "go: running " desc " …"))
  (gt-call-async
   (append (list "test") args (gt-test-flags))
   (lambda (res)
     (set! *gtp-running* #f)
     (set! *gtp-result* res)
     (gtp-auto-expand-failures!)
     (set! *gtp-cursor* 0)
     (set! *gtp-window-start* 0)
     (gtp-rebuild-rows!)
     (set! *gtp-status* (gt-summarize res desc))
     (set-status! (string-append "go: " (gt-summarize res desc))))
   (lambda (msg)
     (set! *gtp-running* #f)
     (set! *gtp-status* (string-append "error: " msg))
     (gtp-rebuild-rows!)
     (set-error! (string-append "go: " msg)))))

(provide go-test)
;;@doc
;; Run the test under the cursor, or the package tests.
(define (go-test)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (let ([line (gt-cursor-line)])
        (gt-call-async
         (list "enclosing" "-file" path "-line" (to-string line))
         (lambda (res)
           (let ([tname (gt-get res 'test_name "")]
                 [dir   (gt-get res 'dir (gt-parent-path path))])
             (if (> (string-length tname) 0)
                 (gt-run-tests! tname
                                (list "-dir" dir "-pkg" "."
                                      "-run" (string-append "^" tname "$")))
                 (gt-run-tests! (string-append "package " (gt-get res 'package_name "?"))
                                (list "-dir" dir "-pkg" ".")))))))))

(provide go-test-package)
;;@doc
;; Run the current file's package tests.
(define (go-test-package)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (gt-run-tests! "package tests"
                     (list "-dir" (gt-parent-path path) "-pkg" "."))))

(provide go-test-all)
;;@doc
;; Run every test in the module.
(define (go-test-all)
  (define path (gt-current-path))
  (define start-dir
    (if (gt-go-file? path) (gt-parent-path path) (static.get-helix-cwd)))
  (define root (and (string? start-dir) (gt-module-root start-dir)))
  (if (not root)
      (set-error! "go: no go.mod found upward from here")
      (gt-run-tests! "all tests" (list "-dir" root "-pkg" "./..."))))

(provide go-test-rerun)
;;@doc
;; Repeat the previous test run.
(define (go-test-rerun)
  (if *gt-last-run*
      (gt-run-tests! (car *gt-last-run*) (cdr *gt-last-run*))
      (set-error! "go: nothing to re-run yet")))

(provide go-test-panel)
;;@doc
;; Toggle/focus the test results panel.
(define (go-test-panel)
  (cond
    [(not *gtp-active*)
     (set! *gtp-active* #t)
     (set! *gtp-focused* #t)
     (gtp-rebuild-rows!)
     (push-component! (gtp-make-bg-component))
     (push-component! (gtp-make-fg-component))]
    [*gtp-focused*
     (set! *gtp-active* #f)
     (set! *gtp-focused* #f)
     (pop-last-component-by-name! "go-test-fg")
     (pop-last-component-by-name! "go-test-bg")
     (set-editor-clip-bottom! 0)]
    [else
     (set! *gtp-focused* #t)
     (gtp-rebuild-rows!)
     (push-component! (gtp-make-fg-component))]))

;; ===== Coverage =====

(define *gt-cover-hints* '())   ; (first-line last-line) ids from add-inlay-hint
(define *gt-cover*       #f)

(define (gt-line-end-char rope line0)
  (with-handler
   (lambda (err) #f)
   (let ([total (text.rope-len-lines rope)])
     (if (< (+ line0 1) total)
         (max 0 (- (text.rope-line->char rope (+ line0 1)) 1))
         #f))))

(define (gt-clear-cover-hints!)
  (for-each
   (lambda (id)
     (with-handler (lambda (err) #f)
                   (remove-inlay-hint-by-id (list-ref id 0) (list-ref id 1))))
   *gt-cover-hints*)
  (set! *gt-cover-hints* '()))

;; Add "not covered" hints to the focused buffer from the file's cover entry.
(define (gt-apply-cover-hints! fentry)
  (define doc-id (editor->doc-id (editor-focus)))
  (define rope (editor->text doc-id))
  (define done (hashset))
  (for-each
   (lambda (b)
     (when (= 0 (gt-int (gt-get b 'count 1)))
       (let* ([line1 (gt-int (gt-get b 'start_line 0))]
              [line0 (- line1 1)]
              [lkey (to-string line1)])
         (unless (or (< line0 0) (hashset-contains? done lkey))
           (set! done (hashset-insert done lkey))
           (let ([char-idx (gt-line-end-char rope line0)])
             (when char-idx
               (with-handler
                (lambda (err) #f)
                (let ([id (add-inlay-hint char-idx "  ● not covered")])
                  (when (list? id)
                    (set! *gt-cover-hints* (cons id *gt-cover-hints*)))))))))))
   (gt-get fentry 'blocks '())))

(define (gt-find-file-cover cover path)
  (let loop ([files (gt-get cover 'files '())])
    (cond [(null? files) #f]
          [(equal? (gt-get (car files) 'file "") path) (car files)]
          [else (loop (cdr files))])))

(provide go-coverage)
;;@doc
;; Run package tests with coverage and mark uncovered lines.
(define (go-coverage)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (let ([dir (gt-parent-path path)])
        (set-status! "go: running tests with coverage …")
        (set! *gtp-running* #t)
        (set! *gtp-status* "running coverage …")
        (gtp-rebuild-rows!)
        (gtp-ensure-open!)
        (gt-call-async
         (append (list "test" "-dir" dir "-pkg" "." "-cover") (gt-test-flags))
         (lambda (res)
           (set! *gtp-running* #f)
           (set! *gtp-result* res)
           (gtp-auto-expand-failures!)
           (gtp-rebuild-rows!)
           (set! *gtp-status* (gt-summarize res "coverage"))
           (let ([cover (hash-try-get res 'cover)])
             (if (not (hash? cover))
                 (set-warning! "go: no coverage data (build failure?)")
                 (begin
                   (set! *gt-cover* cover)
                   (gt-clear-cover-hints!)
                   (let ([fentry (gt-find-file-cover cover (gt-current-path))])
                     (if fentry
                         (begin
                           (gt-apply-cover-hints! fentry)
                           (set-status!
                            (string-append "go: coverage " (gt-pct-str (gt-get cover 'total_pct 0))
                                           " — this file " (gt-pct-str (gt-get fentry 'pct 0))
                                           " (:go-coverage-clear to clear)")))
                         (set-status!
                          (string-append "go: coverage " (gt-pct-str (gt-get cover 'total_pct 0))
                                         " — no statements in this file"))))))))
         (lambda (msg)
           (set! *gtp-running* #f)
           (set! *gtp-status* (string-append "error: " msg))
           (gtp-rebuild-rows!)
           (set-error! (string-append "go: " msg)))))))

(provide go-coverage-clear)
;;@doc
;; Remove coverage marks.
(define (go-coverage-clear)
  (gt-clear-cover-hints!)
  (set-status! "go: coverage marks cleared"))

;; ===== Codegen: struct tags =====

;; Save the buffer, run a tags operation, reload.
(define (gt-tags-op! op tags options)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (let ([line (gt-cursor-line)])
        (helix.write)
        (gt-call-async
         (append (list "tags" "-file" path "-line" (to-string line) "-op" op
                       "-transform" *gt-tags-transform*)
                 (if (> (string-length tags) 0) (list "-tags" tags) '())
                 (if (> (string-length options) 0) (list "-options" options) '()))
         (lambda (res)
           (helix.reload)
           (set-status!
            (string-append "go: " op " tags on "
                           (to-string (gt-int (gt-get res 'fields 0))) " field(s)")))))))

(provide go-tags-add)
;;@doc
;; Add struct field tags, e.g. :go-tags-add json,yaml omitempty
(define (go-tags-add . args)
  (cond
    [(null? args)
     (push-component!
      (prompt "Add tags (e.g. json,yaml or json omitempty): "
              (lambda (input)
                (let ([parts (filter (lambda (s) (> (string-length s) 0))
                                     (split-many input " "))])
                  (unless (null? parts)
                    (gt-tags-op! "add" (car parts)
                                 (if (null? (cdr parts)) "" (cadr parts))))))))]
    [else
     (gt-tags-op! "add" (car args) (if (null? (cdr args)) "" (cadr args)))]))

(provide go-tags-remove)
;;@doc
;; Remove struct field tags, e.g. :go-tags-remove json
(define (go-tags-remove . args)
  (cond
    [(null? args)
     (push-component!
      (prompt "Remove tags (e.g. json,yaml): "
              (lambda (input)
                (when (> (string-length input) 0)
                  (gt-tags-op! "remove" input "")))))]
    [else (gt-tags-op! "remove" (car args) "")]))

(provide go-tags-clear)
;;@doc
;; Clear all tags on the struct under the cursor.
(define (go-tags-clear)
  (gt-tags-op! "clear" "" ""))

;; ===== Codegen: interface stubs =====

(define (gt-impl-run! recv iface)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (begin
        (set-status! (string-append "go: resolving " iface " …"))
        (gt-call-async
         (list "impl" "-file" path "-recv" recv "-iface" iface)
         (lambda (res)
           (let ([code (gt-get res 'code "")])
             (static.insert_string code)
             (set-status!
              (string-append "go: inserted " (to-string (gt-int (gt-get res 'methods 0)))
                             " method stub(s) — check imports"))))))))

(define (gt-impl-parse-and-run! input)
  (let ([parts (filter (lambda (s) (> (string-length s) 0)) (split-many input " "))])
    (if (< (length parts) 2)
        (set-error! "go: expected <receiver> <interface>, e.g. s *Server io.Reader")
        (let* ([n (length parts)]
               [iface (list-ref parts (- n 1))]
               [recv (string-join (gt-take parts (- n 1)) " ")])
          (gt-impl-run! recv iface)))))

(provide go-impl)
;;@doc
;; Insert interface method stubs, e.g. :go-impl s *Server io.Reader
(define (go-impl . args)
  (if (null? args)
      (push-component!
       (prompt "Implement (e.g. s *Server io.Reader): " gt-impl-parse-and-run!))
      (gt-impl-parse-and-run! (string-join args " "))))

;; ===== Codegen: test skeletons =====

(provide go-gotests)
;;@doc
;; Generate a table-driven test for the function under the cursor.
(define (go-gotests)
  (define path (gt-current-path))
  (if (not (gt-go-file? path))
      (set-error! "go: focused buffer is not a Go file")
      (let ([line (gt-cursor-line)])
        (helix.write)
        (gt-call-async
         (list "gotests" "-file" path "-line" (to-string line))
         (lambda (res)
           (let ([tfile (gt-get res 'test_file "")]
                 [tname (gt-get res 'test_name "")]
                 [tline (gt-int (gt-get res 'line 1))]
                 [already? (gt-get res 'already #f)])
             (enqueue-thread-local-callback
              (lambda ()
                (helix.open tfile)
                (when (> tline 0) (helix.goto (to-string tline)))))
             (set-status!
              (if already?
                  (string-append "go: " tname " already exists")
                  (string-append "go: generated " tname)))))))))

;; ===== go.mod dependency panel =====

(define *gm-modules*      '())
(define *gm-cursor*       0)
(define *gm-window-start* 0)
(define *gm-vh*           10)
(define *gm-status*       "")
(define *gm-busy*         #f)
(define *gm-dir*          #f)
(define *gm-open*         #f)

(struct GtModState ())

(define (gm-updatable)
  (filter (lambda (m) (> (string-length (gt-get m 'new_version "")) 0)) *gm-modules*))

(define (gm-module-row m width)
  (define path (gt-get m 'path "?"))
  (define ver  (gt-get m 'version ""))
  (define new  (gt-get m 'new_version ""))
  (define replaced (gt-get m 'replaced ""))
  (define marker (if (> (string-length new) 0) "↑ " "  "))
  (define suffix
    (cond [(> (string-length replaced) 0) (string-append "  ⇒ " replaced)]
          [(> (string-length new) 0) (string-append "  → " new)]
          [else ""]))
  (define indirect (if (gt-get m 'indirect #f) "  (indirect)" ""))
  (gt-truncate
   (string-append marker (gt-pad-right path (min 48 (max 20 (- width 30))))
                  " " ver suffix indirect)
   (- width 4)))

(define (gm-render state rect frame)
  (define W (area-width rect))
  (define H (area-height rect))
  (define bw (min 96 (max 44 (- W 8))))
  (define bh (min (+ (length *gm-modules*) 4) (max 8 (- H 4))))
  (define x0 (gt-div2 (- W bw)))
  (define y0 (gt-div2 (- H bh)))
  (set! *gm-vh* (max 1 (- bh 3)))

  (define bg  (theme-scope-ref "ui.background"))
  (define brd (theme-scope-ref "ui.window"))
  (define txt (theme-scope-ref "ui.text"))
  (define ttl (theme-scope-ref "ui.statusline.normal"))
  (define hl  (theme-scope-ref "ui.menu.selected"))
  (define up-st (gt-theme "diff.plus" "ui.text"))

  (define panel-area (area x0 y0 bw bh))
  (buffer/clear-with frame panel-area bg)
  (block/render frame panel-area (make-block bg brd "all" "rounded"))
  (frame-set-string! frame (+ x0 2) y0 "  go.mod  " ttl)

  (define visible (gt-take (gt-drop *gm-modules* *gm-window-start*) *gm-vh*))
  (let loop ([items visible] [row 0])
    (unless (or (null? items) (>= row *gm-vh*))
      (let* ([m (car items)]
             [abs-idx (+ *gm-window-start* row)]
             [y (+ y0 1 row)]
             [selected? (= abs-idx *gm-cursor*)]
             [updatable? (> (string-length (gt-get m 'new_version "")) 0)])
        (when selected?
          (frame-set-string! frame (+ x0 1) y (make-string (- bw 2) #\space) hl))
        (frame-set-string! frame (+ x0 1) y (gm-module-row m bw)
                           (cond [selected? hl]
                                 [updatable? up-st]
                                 [else txt])))
      (loop (cdr items) (+ row 1))))

  (frame-set-string! frame (+ x0 2) (- (+ y0 bh) 1)
                     (gt-truncate (string-append " " *gm-status* " — ? for help ") (- bw 4))
                     ttl))

(define (gm-cursor-down!)
  (let ([n (length *gm-modules*)])
    (when (< *gm-cursor* (- n 1))
      (set! *gm-cursor* (+ *gm-cursor* 1))
      (when (> *gm-cursor* (+ *gm-window-start* (- *gm-vh* 1)))
        (set! *gm-window-start* (+ *gm-window-start* 1))))))

(define (gm-cursor-up!)
  (when (> *gm-cursor* 0)
    (set! *gm-cursor* (- *gm-cursor* 1))
    (when (< *gm-cursor* *gm-window-start*)
      (set! *gm-window-start* *gm-cursor*))))

(define (gm-current-module)
  (and (not (null? *gm-modules*))
       (< *gm-cursor* (length *gm-modules*))
       (list-ref *gm-modules* *gm-cursor*)))

(define (gm-refresh! check-updates?)
  (set! *gm-busy* #t)
  (set! *gm-status* (if check-updates? "checking for updates …" "loading …"))
  (gt-call-async
   (list "mod" "-dir" *gm-dir* "-action" "list"
         (if check-updates? "-check-updates=true" "-check-updates=false"))
   (lambda (res)
     (set! *gm-busy* #f)
     (when *gm-open*
       (set! *gm-modules*
             (filter (lambda (m) (not (gt-get m 'main #f)))
                     (gt-get res 'modules '())))
       (let ([n (length *gm-modules*)]
             [u (length (gm-updatable))]
             [checked? (gt-get res 'updates_checked #f)]
             [note (gt-get res 'note "")])
         (set! *gm-cursor* (min *gm-cursor* (max 0 (- n 1))))
         (set! *gm-status*
               (string-append (to-string n) " dependencies — "
                              (cond [checked? (string-append (to-string u) " update(s) available")]
                                    [(> (string-length note) 0) (gt-truncate note 60)]
                                    [else "updates unknown (offline?)"]))))))
   (lambda (msg)
     (set! *gm-busy* #f)
     (set! *gm-status* (string-append "error: " msg)))))

(define (gm-upgrade-chain! paths on-done)
  (if (null? paths)
      (on-done)
      (begin
        (set! *gm-status* (string-append "upgrading " (car paths) " …"))
        (gt-call-async
         (list "mod" "-dir" *gm-dir* "-action" "upgrade" "-module" (car paths))
         (lambda (res)
           (if (gt-get res 'ok #f)
               (gm-upgrade-chain! (cdr paths) on-done)
               (begin
                 (set! *gm-busy* #f)
                 (set! *gm-status*
                       (gt-truncate (string-append "error: " (gt-get res 'output "upgrade failed")) 80)))))
         (lambda (msg)
           (set! *gm-busy* #f)
           (set! *gm-status* (string-append "error: " msg)))))))

(define (gm-upgrade-selected!)
  (define m (gm-current-module))
  (when (and m (not *gm-busy*))
    (set! *gm-busy* #t)
    (gm-upgrade-chain! (list (gt-get m 'path ""))
                       (lambda () (gm-refresh! #t)))))

(define (gm-upgrade-all!)
  (define paths (map (lambda (m) (gt-get m 'path "")) (gm-updatable)))
  (cond
    [(null? paths) (set! *gm-status* "nothing to upgrade")]
    [*gm-busy* void]
    [else
     (set! *gm-busy* #t)
     (gm-upgrade-chain! paths (lambda () (gm-refresh! #t)))]))

(define (gm-tidy!)
  (unless *gm-busy*
    (set! *gm-busy* #t)
    (set! *gm-status* "go mod tidy …")
    (gt-call-async
     (list "mod" "-dir" *gm-dir* "-action" "tidy")
     (lambda (res)
       (set! *gm-busy* #f)
       (if (gt-get res 'ok #f)
           (gm-refresh! #t)
           (set! *gm-status*
                 (gt-truncate (string-append "error: " (gt-get res 'output "tidy failed")) 80))))
     (lambda (msg)
       (set! *gm-busy* #f)
       (set! *gm-status* (string-append "error: " msg))))))

(define *gm-help*
  (list "j/k       navigate"
        "u         upgrade selected to @latest"
        "U         upgrade all with updates"
        "t         go mod tidy"
        "R         refresh (re-check updates)"
        "q/esc     close"))

(define (gm-handle-event state event)
  (cond
    [(not (key-event? event)) event-result/ignore]
    [else
     (define ch (key-event-char event))
     (cond
       [(or (key-event-escape? event) (and (char? ch) (equal? ch #\q)))
        (set! *gm-open* #f)
        event-result/close]
       [(or (key-event-down? event) (and (char? ch) (equal? ch #\j)))
        (gm-cursor-down!) event-result/consume]
       [(or (key-event-up? event) (and (char? ch) (equal? ch #\k)))
        (gm-cursor-up!) event-result/consume]
       [(and (char? ch) (equal? ch #\g)) (set! *gm-cursor* 0) (set! *gm-window-start* 0) event-result/consume]
       [(and (char? ch) (equal? ch #\G))
        (let ([n (length *gm-modules*)])
          (when (> n 0)
            (set! *gm-cursor* (- n 1))
            (set! *gm-window-start* (max 0 (- n *gm-vh*)))))
        event-result/consume]
       [(and (char? ch) (equal? ch #\u)) (gm-upgrade-selected!) event-result/consume]
       [(and (char? ch) (equal? ch #\U)) (gm-upgrade-all!) event-result/consume]
       [(and (char? ch) (equal? ch #\t)) (gm-tidy!) event-result/consume]
       [(and (char? ch) (equal? ch #\R)) (gm-refresh! #t) event-result/consume]
       [(and (char? ch) (equal? ch #\?))
        (gt-show-help! "go.mod Panel" *gm-help*) event-result/consume]
       [else event-result/consume])]))

(provide go-mod-panel)
;;@doc
;; Open the go.mod dependency panel (upgrade with u/U, tidy with t).
(define (go-mod-panel)
  (define path (gt-current-path))
  (define start-dir
    (if (string? path) (gt-parent-path path) (static.get-helix-cwd)))
  (define root (and (string? start-dir) (gt-module-root start-dir)))
  (if (not root)
      (set-error! "go: no go.mod found upward from here")
      (begin
        (set! *gm-dir* root)
        (set! *gm-open* #t)
        (set! *gm-cursor* 0)
        (set! *gm-window-start* 0)
        (set! *gm-modules* '())
        (push-component!
         (new-component! "go-mod-panel" (GtModState) gm-render
                         (hash "handle_event" gm-handle-event)))
        (gm-refresh! #t))))
