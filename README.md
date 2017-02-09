# rush

[![Build Status](https://travis-ci.org/shenwei356/rush.svg?branch=master)](https://travis-ci.org/shenwei356/rush)
[![Built with GoLang](https://img.shields.io/badge/powered_by-go-6362c2.svg?style=flat)](https://golang.org)
[![Go Report Card](https://goreportcard.com/badge/github.com/shenwei356/rush)](https://goreportcard.com/report/github.com/shenwei356/rush)
[![Cross-platform](https://img.shields.io/badge/platform-any-ec2eb4.svg?style=flat)](#download)
[![Latest Version](https://img.shields.io/github/release/shenwei356/rush.svg?style=flat?maxAge=86400)](https://github.com/shenwei356/rush/releases)
[![Github Releases](https://img.shields.io/github/downloads/shenwei356/rush/latest/total.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases)

`rush` -- parallelly execute shell commands. A GNU parallel like tool in Go.

`rush` is a tool similar to [GNU parallel](https://www.gnu.org/software/parallel/)
 and [gargs](https://github.com/brentp/gargs).
 `rush` borrows some idea from them and has some unique features,
  e.g., more advanced embeded strings replacement than `parallel`.

## Table of Contents
<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->

- [Features](#features)
- [Performance](#performance)
- [Examples](#examples)
- [Usage](#usage)
- [Installation](#installation)
- [Download](#download)
- [Special Cases](#special-cases)
- [Acknowledgements](#acknowledgements)
- [Contact](#contact)
- [License](#license)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->


## Features

Major:

- **Avoid mixed line from multiple processes without loss of performance**,
  e.g. the first half of a line is from one process
  and the last half of the line is from another process.
  Similar with `parallel --line-buffer`
- **Timeout** (`-t`)
- **Retry** (`-r`)
- **Safe exit after capturing Ctrl-C**
- **Continue** (`-c`)
- **`awk -v` like custom defined variables** (`-v`)
- **Keeping output in order of input** (`-k`)
- **Exit on first error(s)** (`-e`)
- **Settable record delimiter** (`-D`, default `\n`),
- **Settable records sending to every command** (`-n`, default `1`)
- **Settable field delimiter** (`-d`, default `\s+`)
- **Practical replacement strings** (like GNU parallel):
    - `{#}`, job ID
    - `{}`, full data
    - `{n}`, `n`th field in delimiter-delimited data
    - Directory and file
        - `{/}`, dirname  (`{//}` in GNU parallel)
        - `{%}`, basename (`{/}` in GNU parallel)
        - `{.}`, remove the last extension
        - `{:}`, remove any extension (GNU parallel does not have)
        - `{^suffix}`, remove `suffix` (GNU parallel does not have)
    - Combinations:
        - `{%.}`, `{%:}`, basename without extension
        - `{2.}`, `{2/}`, `{2%.}`, manipulate `n`th field

Minor:

- Dry run (`--dry-run`)
- Trim input data (`--trim`)
- Verbose output (`--verbose`)

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

        $ rush echo {} -i data1.txt -i data2.txt

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

1. Dirname (`{/}`) and basename (`{%}`) and remove custom suffix (`{^suffix}`)

        $ echo dir/file_1.txt.gz | rush 'echo {/} {%} {^_1.txt.gz}'
        dir file_1.txt.gz dir/file

1. Get basename, and remove last (`{.}`) or any (`{:}`) extension

        $ echo dir.d/file.txt.gz | rush 'echo {.} {:} {%.} {%:}'
        dir.d/file.txt dir.d/file file.txt file

1. Job ID, combine fields index and other replacement strings

        $ echo 12 file.txt dir/s_1.fq.gz | rush 'echo job {#}: {2} {2.} {3%:^_1}'
        job 1: file.txt file s

1. Custom field delimiter (`-d`)

        $ echo a=b=c | rush 'echo {1} {2} {3}' -d =
        a b c

1. Send multi-lines to every command (`-n`)

        $ seq 5 | rush -n 2 -k 'echo "{}"; echo'
        1
        2

        3
        4

        5

        # Multiple records are joined with separator `"\n"` (`-J/--records-join-sep`)
        $ seq 5 | rush -n 2 -k 'echo "{}"; echo' -J ' '
        1 2

        3 4

        5

        $ seq 5 | rush -n 2 -k -j 3 'echo {1}'
        1
        3
        5

1. Custom record delimiter (`-D`), note that empty records are not used.

        $ echo -ne ">seq1\nactg\n>seq2\nAAAA\n>seq3\nCCCC"
        >seq1
        actg
        >seq2
        AAAA
        >seq3
        CCCC

        $ echo -ne ">seq1\nactg\n>seq2\nAAAA\n>seq3\nCCCC" | rush -D ">" 'echo FASTA record {#}: name: {1} sequence: {2}' -k -d "\n"
        FASTA record 1: name: seq1 sequence: actg
        FASTA record 2: name: seq2 sequence: AAAA
        FASTA record 3: name: seq3 sequence: CCCC

1. Assign value to variable, like `awk -v` (`-v`)

        $ seq 1  | rush 'echo Hello, {fname} {lname}!' -v fname=Wei -v lname=Shen
        Hello, Wei Shen!

        $ for var in a b; do \
        $   seq 1 3 | rush -k -v var=$var 'echo var: {var}, data: {}'; \
        $ done
        var: a, data: 1
        var: a, data: 2
        var: a, data: 3
        var: b, data: 1
        var: b, data: 2
        var: b, data: 3

        $ seq 2 | rush ' seq 3 | awk -v s={} "{print \"source: \" s \", data: \" \$1}" '
        source: 1, data: 1
        source: 1, data: 2
        source: 1, data: 3
        source: 2, data: 1
        source: 2, data: 2
        source: 2, data: 3

1. Interrupt jobs by `Ctrl-C`, rush will stop unfinished commands and exit.

        $ seq 1 20 | rush 'sleep 1; echo {}'
        ^C[CRIT] received an interrupt, stopping unfinished commands...
        [ERRO] wait cmd #7: sleep 1; echo 7: signal: interrupt
        [ERRO] wait cmd #5: sleep 1; echo 5: signal: killed
        [ERRO] wait cmd #6: sleep 1; echo 6: signal: killed
        [ERRO] wait cmd #8: sleep 1; echo 8: signal: killed
        [ERRO] wait cmd #9: sleep 1; echo 9: signal: killed
        1
        3
        4
        2

1. Continue/resume jobs (`-c`). When some jobs failed (by execution failure, timeout,
    or cancelling by user with `Ctrl + C`),
    please switch flag `-c/--continue` on and run again,
    so that `rush` can save successful commands and ignore them in **NEXT** run.

        $ seq 1 3 | rush 'sleep {}; echo {}' -t 3 -c
        1
        2
        [ERRO] run cmd #3: sleep 3; echo 3: time out

        # successful commands:
        $ cat successful_cmds.rush
        sleep 1; echo 1__CMD__
        sleep 2; echo 2__CMD__

        # run again
        $ seq 1 3 | rush 'sleep {}; echo {}' -t 3 -c
        [INFO] ignore cmd #1: sleep 1; echo 1
        [INFO] ignore cmd #2: sleep 2; echo 2
        [ERRO] run cmd #1: sleep 3; echo 3: time out

    Commands of multi-lines

        $ seq 1 3 | rush 'sleep {}; echo {}; \
        echo finish {}' -t 3 -c -C finished.rush
        1
        finish 1
        2
        finish 2
        [ERRO] run cmd #3: sleep 3; echo 3; \
        echo finish 3: time out

        $ cat finished.rush
        sleep 1; echo 1; \
        echo finish 1__CMD__
        sleep 2; echo 2; \
        echo finish 2__CMD__

        # run again
        $ seq 1 3 | rush 'sleep {}; echo {}; \
        echo finish {}' -t 3 -c -C finished.rush
        [INFO] ignore cmd #1: sleep 1; echo 1; \
        echo finish 1
        [INFO] ignore cmd #2: sleep 2; echo 2; \
        echo finish 2
        [ERRO] run cmd #1: sleep 3; echo 3; \
        echo finish 3: time out

    Commands are saved to file (`-C`) right after it finished, so we can view
    the check finished jobs:

        grep -c __CMD__ successful_cmds.rush

1. A comprehensive example: downloading 1K+ pages given by three URL list files
   using `phantomjs save_page.js` (some page contents are dynamicly generated by Javascript,
   so `wget` does not work). Here I set max jobs number (`-j`) as `20`,
   each job has a max running time (`-t`) of `60` seconds and `3` retry changes
   (`-r`). Continue flag `-c` is also switched on, so we can continue unfinished
   jobs. Luckily, it's accomplished in one run :smile:

        $ for f in $(seq 2014 2016); do \
        $    /bin/rm -rf $f; mkdir -p $f; \
        $    cat $f.html.txt | rush -v d=$f -d = 'phantomjs save_page.js "{}" > {d}/{3}.html' -j 20 -t 60 -r 3 -c; \
        $ done

1. A bioinformatics example: mapping with `bwa`, and processing result with `samtools`:

        $ tree raw.cluster.clean.mapping
        raw.cluster.clean.mapping
        ├── M1
        │   ├── M1_1.fq.gz -> ../../raw.cluster.clean/M1/M1_1.fq.gz
        │   ├── M1_2.fq.gz -> ../../raw.cluster.clean/M1/M1_2.fq.gz
        ...

        $ ref=ref/xxx.fa
        $ threads=25
        $ ls -d raw.cluster.clean.mapping/* | rush -v ref=$ref -v j=$threads \
            'bwa mem -t {j} -M -a {ref} {}/{%}_1.fq.gz {}/{%}_2.fq.gz > {}/{%}.sam; \
            samtools view -bS {}/{%}.sam > {}/{%}.bam; \
            samtools sort -T {}/{%}.tmp -@ {j} {}/{%}.bam -o {}/{%}.sorted.bam; \
            samtools index {}/{%}.sorted.bam; \
            samtools flagstat {}/{%}.sorted.bam > {}/{%}.sorted.bam.flagstat; \
            /bin/rm {}/{%}.bam {}/{%}.sam;' \
            -j 2 --verbose -c -C mapping.rush

## Usage

```
rush -- parallelly execute shell commands

Version: 0.1.5

Author: Wei Shen <shenwei356@gmail.com>

Source code: https://github.com/shenwei356/rush

Usage:
  rush [flags] [command] [args of command...]

Examples:
  1. simple run, quoting is not necessary
      $ seq 1 10 | rush echo {}
  2. keep order
      $ seq 1 10 | rush 'echo {}' -k
  3. timeout
      $ seq 1 | rush 'sleep 2; echo {}' -t 1
  4. retry
      $ seq 1 | rush 'python script.py' -r 3
  5. dirname & basename & remove suffix
      $ echo dir/file_1.txt.gz | rush 'echo {/} {%} {^_1.txt.gz}'
      dir file.txt.gz dir/file
  6. basename without last or any extension
      $ echo dir.d/file.txt.gz | rush 'echo {.} {:} {%.} {%:}'
      dir.d/file.txt dir.d/file file.txt file
  7. job ID, combine fields and other replacement strings
      $ echo 12 file.txt dir/s_1.fq.gz | rush 'echo job {#}: {2} {2.} {3%:^_1}'
      job 1: file.txt file s
  8. custom field delimiter
      $ echo a=b=c | rush 'echo {1} {2} {3}' -d =
      a b c
  9. assign value to variable, like "awk -v"
      $ seq 1 | rush 'echo Hello, {fname} {lname}!' -v fname=Wei -v lname=Shen
      Hello, Wei Shen!
  10. save successful commands to continue in NEXT run
      $ seq 1 3 | rush 'sleep {}; echo {}' -c -t 2
      [INFO] ignore cmd #1: sleep 1; echo 1
      [ERRO] run cmd #1: sleep 2; echo 2: time out
      [ERRO] run cmd #2: sleep 3; echo 3: time out

  More examples: https://github.com/shenwei356/rush

Flags:
  -v, --assign stringSlice        assign the value val to the variable var (format: var=val)
  -c, --continue                  continue jobs. NOTES: 1) successful commands are saved in file (given by flag -C/--succ-cmd-file); 2) if the file does not exist, rush saves data so we can continue jobs next time; 3) if the file exists, rush ignores jobs in it and update the file
      --dry-run                   print command but not run
  -d, --field-delimiter string    field delimiter in records, support regular expression (default "\s+")
  -i, --infile stringSlice        input data file, multi-values supported
  -j, --jobs int                  run n jobs in parallel (default value depends on your device) (default 4)
  -k, --keep-order                keep output in order of input
  -n, --nrecords int              number of records sent to a command (default 1)
  -o, --out-file string           out file ("-" for stdout) (default "-")
  -D, --record-delimiter string   record delimiter (default is "\n") (default "
")
  -J, --records-join-sep string   record separator for joining multi-records (default is "\n") (default "
")
  -r, --retries int               maximum retries (default 0)
      --retry-interval int        retry interval (unit: second) (default 0)
  -e, --stop-on-error             stop all processes on first error(s)
  -C, --succ-cmd-file string      file for saving successful commands (default "successful_cmds.rush")
  -t, --timeout int               timeout of a command (unit: second, 0 for no timeout) (default 0)
      --trim string               trim white space in input (available values: "l" for left, "r" for right, "lr", "rl", "b" for both side)
      --verbose                   print verbose information
  -V, --version                   print version information and check for update

```

## Installation

`rush` is implemented in [Go](https://golang.org/) programming language,
 executable binary files **for most popular operating systems** are freely available
  in [release](https://github.com/shenwei356/rush/releases) page.

#### Method 1: Download binaries

Just [download](https://github.com/shenwei356/rush/releases) compressed
executable file of your operating system,
and decompress it with `tar -zxvf *.tar.gz` command or other tools.
And then:

1. **For Linux-like systems**
    1. If you have root privilege simply copy it to `/usr/local/bin`:

            sudo cp rush /usr/local/bin/

    1. Or add the current directory of the executable file to environment variable
    `PATH`:

            echo export PATH=\$PATH:\"$(pwd)\" >> ~/.bashrc
            source ~/.bashrc


1. **For windows**, just copy `rush.exe` to `C:\WINDOWS\system32`.

#### Method 2: For Go developer

    go get -u github.com/shenwei356/rush/

## Download

[rush v0.1.5](https://github.com/shenwei356/rush/releases/tag/v0.1.5)
[![Github Releases (by Release)](https://img.shields.io/github/downloads/shenwei356/rush/v0.1.5/total.svg)](https://github.com/shenwei356/rush/releases/tag/v0.1.5)

OS     |Arch      |File                                                                                                                          |Download Count
:------|:---------|:-----------------------------------------------------------------------------------------------------------------------------|:-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------
Linux  |32-bit    |[rush_linux_386.tar.gz](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_linux_386.tar.gz)                    |[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_linux_386.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_linux_386.tar.gz)
Linux  |**64-bit**|[**rush_linux_amd64.tar.gz**](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_linux_amd64.tar.gz)            |[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_linux_amd64.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_linux_amd64.tar.gz)
OS X   |32-bit    |[rush_darwin_386.tar.gz](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_darwin_386.tar.gz)                  |[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_darwin_386.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_darwin_386.tar.gz)
OS X   |**64-bit**|[**rush_darwin_amd64.tar.gz**](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_darwin_amd64.tar.gz)          |[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_darwin_amd64.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_darwin_amd64.tar.gz)
Windows|32-bit    |[rush_windows_386.exe.tar.gz](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_windows_386.exe.tar.gz)        |[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_windows_386.exe.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_windows_386.exe.tar.gz)
Windows|**64-bit**|[**rush_windows_amd64.exe.tar.gz**](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_windows_amd64.exe.tar.gz)|[![Github Releases (by Asset)](https://img.shields.io/github/downloads/shenwei356/rush/latest/rush_windows_amd64.exe.tar.gz.svg?maxAge=3600)](https://github.com/shenwei356/rush/releases/download/v0.1.5/rush_windows_amd64.exe.tar.gz)

## Special Cases

- Shell `grep` returns exit code `1` when no matches found.
`rush` thinks it failed to run.
 Please use `grep foo bar || true` instead of `grep foo bar`.

        $ seq 1 | rush 'echo abc | grep 123'
        [ERRO] wait cmd #1: echo abc | grep 123: exit status 1
        $ seq 1 | rush 'echo abc | grep 123 || true'



## Acknowledgements

Specially thank [@brentp](https://github.com/brentp)
and his [gargs](https://github.com/brentp/gargs), from which `rush` borrows
some ideas.

## Contact

[Create an issue](https://github.com/shenwei356/rush/issues) to report bugs,
propose new functions or ask for help.

## License

[MIT License](https://github.com/shenwei356/rush/blob/master/LICENSE)
