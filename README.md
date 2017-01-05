# rush

`rush` -- parallelly execute shell commands.

`rush` is a tool similar to [GNU parallel](https://www.gnu.org/software/parallel/)
 and [gargs](https://github.com/brentp/gargs).
 `rush` borrows some idea from them and has some unique features,
  e.g., more advanced embeded strings replacement than `parallel`.

**Source code:** [https://github.com/shenwei356/rush](https://github.com/shenwei356/rush)
[![GitHub stars](https://img.shields.io/github/stars/shenwei356/rush.svg?style=social&label=Star&?maxAge=2592000)](https://github.com/shenwei356/rush)
[![license](https://img.shields.io/github/license/shenwei356/rush.svg?maxAge=2592000)](https://github.com/shenwei356/rush/blob/master/LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/shenwei356/rush)](https://goreportcard.com/report/github.com/shenwei356/rush)

**Latest version:** [![Latest Version](https://img.shields.io/github/release/shenwei356/rush.svg?style=flat?maxAge=86400)](https://github.com/shenwei356/rush/releases)
[![Github Releases](https://img.shields.io/github/downloads/shenwei356/rush/latest/total.svg?maxAge=3600)](http://bioinf.shenwei.me/rush/download/)

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
    - [x] matches of regular expression
- [x] GNU parallel like replacement strings:
    - [x] `{#}`, job number
    - [x] `{}`, full line
    - [x] `{.}`, input line without the last extension
    - [x] `{:}`, input line without any extension (GNU parallel does not have)
    - [x] `{/}`, dirname  (`{//}` in GNU parallel)
    - [x] `{%}`, basename (`{/}` in GNU parallel)
    - [x] possible combinations:
        - [x] `{%.}`, `{%,}`
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

See on [release page](https://github.com/shenwei356/rush/releases)

## Acknowledgements

Specially thank [@brentp](https://github.com/brentp)
and his [gargs](https://github.com/brentp/gargs), from which `rush` borrows
lots of ideas.

## Contact

[Create an issue](https://github.com/shenwei356/rush/issues) to report bugs,
propose new functions or ask for help.

## License

[MIT License](https://github.com/shenwei356/rush/blob/master/LICENSE)
