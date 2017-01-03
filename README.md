# rush -- parallelly execute shell commands


## Features

Major:

- [ ] record delimiter (`-D`, default `\n`),
  records sending to every command (`-n`, default `1`),
  and field delimiter (`-d`, default `\s+`).
- [ ] keep output order, may use temporary file
- [ ] support timeout and retry
- [ ] support continue,
  save status after [capturing ctrl+c](https://nathanleclaire.com/blog/2014/08/24/handling-ctrl-c-interrupt-signal-in-golang-programs/)
- [ ] support positional replacement strings: `{n}`
    - [ ] columns in delimiter-delimited data
    - [ ] matches of regular expression
- [ ] GNU parallel like replacement strings:
    - [ ] `{@}`, job slot number (`{%}` in GNU parallel)
    - [ ] `{#}`, job number
    - [ ] `{}`, full line
    - [ ] `{.}`, input line without the last extension
    - [ ] `{,}`, input line without any extension (GNU parallel does not have)
    - [ ] `{/}`, dirname  (`{//}` in GNU parallel)
    - [ ] `{%}`, basename (`{/}` in GNU parallel)
    - [ ] possible combinations:
        - [ ] `{%.}`, `{%,}`
        - [ ] `{n.}`, `{n/}` ...
- [ ] `awk -v` like defined variables
- [ ] appropriate quoting

Minor:

- [ ] logging
- [ ] dry run
- [ ] exit on error
- [ ] trim arguments
- [ ] verbose

## Workflow

1. read data from STDIN (default) or files (`-i`)
1. split N (`-n`, default `1`) records by record delimiter (`-D`, default `\n`) into Chunks
1. send splitted data to W workers (`-j`, default `#. CPUs`)
1. workers run commands in parallel
    1. optinally split input data by field delimiter (`-d`, default `\s+`)
    1. worker replaces placeholders in command (by joining arguments) with input data
    1. optionally (`--dry-run`) print command and not run
    1. retry if fail to run, give up when reached the maximum retry times (`-r`)
    1. cancel if time out (`-t`)
    1. optionally (`-e`) stop all worers and exit if error occured
    1. output failed comands to file (`failed.txt`),
       so we can redo them (`rush -i failed.txt`)
    1. optionally (`-c`) save finished commands to file (`finished.txt`),
       so we can ignore them when run in "continue" mode (`-c`)
1. show STDOUT of commands to STDOUT,
   optionally (`-k`) keep order according to the input data
1. show simple summary when all data processed

## Contact

[Create an issue](https://github.com/shenwei356/rush/issues) to report bugs,
propose new functions or ask for help.

## License

[MIT License](https://github.com/shenwei356/rush/blob/master/LICENSE)
