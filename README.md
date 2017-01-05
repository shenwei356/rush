# rush

[![Go Report Card](https://goreportcard.com/badge/github.com/shenwei356/rush)](https://goreportcard.com/report/github.com/shenwei356/rush)
[![Latest Version](https://img.shields.io/github/release/shenwei356/rush.svg?style=flat?maxAge=86400)](https://github.com/shenwei356/rush/releases)
[![Github Releases](https://img.shields.io/github/downloads/shenwei356/rush/latest/total.svg?maxAge=3600)](http://bioinf.shenwei.me/rush/download/)

`rush` -- parallelly execute shell commands.

`rush` is a tool similar to [GNU parallel](https://www.gnu.org/software/parallel/)
 and [gargs](https://github.com/brentp/gargs).
 `rush` borrows some idea from them and has some unique features,
  e.g., more advanced embeded strings replacement than `parallel`.

## Features

Major:

- [x] record delimiter (`-D`, default `\n`),
  records sending to every command (`-n`, default `1`),
  and field delimiter (`-d`, default `\s+`).
- [x] keep output order, may use temporary file
- [x] support timeout and retry
- [x] exit on fist error
- [ ] support continue,
  save status after [capturing ctrl+c](https://nathanleclaire.com/blog/2014/08/24/handling-ctrl-c-interrupt-signal-in-golang-programs/)
- [x] support positional replacement strings: `{n}`
    - [x] columns in delimiter-delimited data
    - [ ] matches of regular expression
- [x] GNU parallel like replacement strings:
    - [x] `{#}`, job number
    - [x] `{}`, full line
    - [x] `{.}`, input line without the last extension
    - [x] `{:}`, input line without any extension (GNU parallel does not have)
    - [x] `{/}`, dirname  (`{//}` in GNU parallel)
    - [x] `{%}`, basename (`{/}` in GNU parallel)
    - [x] possible combinations:
        - [x] `{%.}`, `{%:}`
        - [x] `{n.}`, `{n/}` ...
- [x] `awk -v` like defined variables
- [ ] appropriate quoting

Minor:

- [x] logging
- [x] dry run
- [x] trim arguments
- [x] verbose

## Workflow

1. read data from STDIN (default) or files (`-i`)
1. split N (`-n`, default `1`) records by record delimiter (`-D`, default `\n`) into Chunks
1. create commands with splitted data
    1. optinally split input data by field delimiter (`-d`, default `\s+`)
    1. replaces placeholders in command (by joining arguments) with input data
1. run commands in parallel (max jobs: `-j`, default `#. CPUs`)
    1. optionally (`--dry-run`) print command and not run
    1. retry if fail to run, give up when reached the max retry times (`-r`)
    1. cancel if time out (`-t`)
    1. optionally (`-e`) stop all commands and exit on first error
    1. output failed comands to file (`failed.txt`), so we can redo them (`rush -i failed.txt`)
    1. optionally (`-c`) save finished commands to file (`finished.txt`),
       so we can ignore them when run in "continue" mode (`-c`)
1. show STDOUT of commands to STDOUT,
   optionally (`-k`) keep order according to the input data

## Performance

See on [release page](https://github.com/shenwei356/rush/releases).

## Usage & Examples

```
rush -- parallelly execute shell commands

Version: 0.0.2

Author: Wei Shen <shenwei356@gmail.com>

Source code: https://github.com/shenwei356/rush

Usage:
  rush [flags] [command] [args of command...]

Examples:
  1. simple run  : seq 1 10 | rush echo {}  # quoting is not necessary
  2. keep order  : seq 1 10 | rush 'echo {}' -k
  3. with timeout: seq 1 | rush 'sleep 2; echo {}' -t 1
  4. retry       : seq 1 | rush 'python script.py' -r 3
  5. basename    : echo dir/file.txt.gz | rush 'echo {%}'　　# file.txt.gz
  6. dirname     : echo dir/file.txt.gz | rush 'echo {/}'    # dir
  7. basename without last extension
                 : echo dir/file.txt.gz | rush 'echo {%.}'   # file.txt
  8. basename without last extension
                 : echo dir/file.txt.gz | rush 'echo {%:}'   # file
  9. job ID, combine fields and other replacement string
                 : echo 123 file.txt | rush 'echo job {#}: {2} {2.} {1}'
                 # job 1: file.txt file 123

Flags:
  -v, --assign stringSlice        assign the value val to the variable var (format: var=val)
      --dry-run                   print command but not run
  -d, --field-delimiter string    field delimiter in records (default "\s+")
  -i, --infile stringSlice        input data file
  -j, --jobs int                  run n jobs in parallel (default 4)
  -k, --keep-order                keep output in order of input
  -n, --nrecords int              number of records sent to a command (default 1)
  -o, --out-file string           out file ("-" for stdout) (default "-")
  -D, --record-delimiter string   record delimiter (default is "\n") (default "
")
  -r, --retries int               maximum retries
      --retry-interval int        retry interval (unit: second)
  -e, --stop-on-error             stop all processes on first error
  -t, --timeout int               timeout of a command (unit: second, 0 for no timeout)
      --trim string               trim white space in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)
      --verbose                   print verbose information
  -V, --version                   print version information and check for update

```

## Acknowledgements

Specially thank [@brentp](https://github.com/brentp)
and his [gargs](https://github.com/brentp/gargs), from which `rush` borrows
lots of ideas.

## Contact

[Create an issue](https://github.com/shenwei356/rush/issues) to report bugs,
propose new functions or ask for help.

## License

[MIT License](https://github.com/shenwei356/rush/blob/master/LICENSE)
