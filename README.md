
# rush

[![Go Report Card](https://goreportcard.com/badge/github.com/shenwei356/rush)](https://goreportcard.com/report/github.com/shenwei356/rush)
[![Latest Version](https://img.shields.io/github/release/shenwei356/rush.svg?style=flat?maxAge=86400)](https://github.com/shenwei356/rush/releases)
[![Github Releases](https://img.shields.io/github/downloads/shenwei356/rush/latest/total.svg?maxAge=3600)](http://bioinf.shenwei.me/rush/download/)

`rush` -- parallelly execute shell commands.

`rush` is a tool similar to [GNU parallel](https://www.gnu.org/software/parallel/)
 and [gargs](https://github.com/brentp/gargs).
 `rush` borrows some idea from them and has some unique features,
  e.g., more advanced embeded strings replacement than `parallel`.

<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->

- [Features](#features)
- [Performance](#performance)
- [Examples](#examples)
- [Usage](#usage)
- [Acknowledgements](#acknowledgements)
- [Contact](#contact)
- [License](#license)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->


## Features

Major:

- [x] record delimiter (`-D`, default `\n`),
  records sending to every command (`-n`, default `1`),
  and field delimiter (`-d`, default `\s+`).
- [x] keep output order, may use temporary file
- [x] support timeout and retry
- [x] support exit on fist error(s)
- [ ] support continue,
  save status after [capturing ctrl+c](https://nathanleclaire.com/blog/2014/08/24/handling-ctrl-c-interrupt-signal-in-golang-programs/)
- [x] Replacement strings (like GNU parallel):
    - [x] `{#}`, job number
    - [x] `{}`, full line
    - [x] support positional replacement strings: `{n}`
        - [x] `n`th field in delimiter-delimited data
        - [ ] `n`th matches of regular expression
    - [x] direcotry and file
        - [x] `{/}`, dirname  (`{//}` in GNU parallel)
        - [x] `{%}`, basename (`{/}` in GNU parallel)
        - [x] `{.}`, remove the last extension
        - [x] `{:}`, remove any extension (GNU parallel does not have)
    - [x] combinations:
        - [x] `{%.}`, `{%:}`, basename without extension
        - [x] `{n.}`, `{n/}`, manipulate nth field
- [x] `awk -v` like defined variables
- [ ] appropriate quoting

Minor:

- [x] logging
- [x] dry run
- [x] trim arguments
- [x] verbose

## Performance

Performance of `rush` is similar to `gargs`, and they are both slightly faster than `parallel` (Perl).
See on [release page](https://github.com/shenwei356/rush/releases).

## Examples

1. Simple run, quoting is not necessary

        # seq 1 3 | rush 'echo {}'
        $ seq 1 3 | rush echo {}
        3
        1
        2

1. Read data from file (`-i`)

        $ rush echo {} -i data.txt

1. Keep output order (`-k`)

        $ seq 1 3 | rush 'echo {}' -k
        1
        2
        3

1. Timeout (`-t`)

        $ time seq 1 | rush 'sleep 2; echo {}' -t 1
        [ERRO] run command #1: sleep 2; echo 1: time out

        real    0m1.010s
        user    0m0.005s
        sys     0m0.007s

1. Retry (`-r`)

        $ seq 1 | rush 'python unexisted_script.py' -r 1
        python: can't open file 'unexisted_script.py': [Errno 2] No such file or directory
        [WARN] wait command: python unexisted_script.py: exit status 2
        python: can't open file 'unexisted_script.py': [Errno 2] No such file or directory
        [ERRO] wait command: python unexisted_script.py: exit status 2

1. Directory (`{/}`) and basename (`{%}`)

        $ echo dir/file.txt.gz | rush 'echo {/} {%}'
        dir file.txt.gz

1. Get basename, and remove last (`{.}`) or any (`{:}`) extension

        $ echo dir/file.txt.gz | rush 'echo {%.} {%:}'
        file.txt file

1. Job ID, combine fields index and other replacement strings

        $ echo 123 file.txt | rush 'echo job {#}: {2} {2.} {1}'
        job 1: file.txt file 123

1. Custom field delimiter (`-d`)

        $ echo a=b=c | rush 'echo {1} {2} {3}' -d =
        a b c

1. Send multi-lines to every command (`-n`)

        $ seq 1 5 | rush -j 3 'echo {1}' -n 2 -k
        1
        3
        5

1. Custom record delimiter (`-D`)

        $ echo -ne ">seq1\nactg\n>seq2\nAAAA\n>seq3\nCCCC"
        >seq1
        actg
        >seq2
        AAAA
        >seq3
        CCCC

        $ echo -ne ">seq1\nactg\n>seq2\nAAAA\n>seq3\nCCCC" | rush -D ">" 'echo FASTA record {#}: name: {1} sequence: {2}' -k -d "\n"
        FASTA record 1: name: sequence:
        FASTA record 2: name: seq1 sequence: actg
        FASTA record 3: name: seq2 sequence: AAAA
        FASTA record 4: name: seq3 sequence: CCCC

## Usage

```
rush -- parallelly execute shell commands

Version: 0.0.3

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
  More examples: https://github.com/shenwei356/rush

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
  -e, --stop-on-error             stop all processes on first error(s)
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
