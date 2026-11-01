#!/usr/bin/env python3

import sys
import time
import os
import shlex
import subprocess
import tempfile


def timeit(cmd):
    t = time.monotonic()
    # Reject failures from either side of the pipeline before reporting timings.
    subprocess.run(["bash", "-o", "pipefail", "-c", cmd], check=True)
    return time.monotonic() - t

# test name, input command, test command
tests = [
    ['a few stdout', 'seq 1 10', 'echo job:{}; seq 1 10'],
    ['lots of stdout', 'seq 1 10', 'echo job:{}; seq 1 1000000'],
    ['a few stdout & run long', 'seq 1 10', 'echo job:{}; sleep 2; seq 1 10'],
    ['lots of stdout & run long', 'seq 1 10', 'echo job:{}; sleep 2; seq 1 1000000'],
]

apps = {'parallel': {'jobs': '-j', 'keep-order': '-k'},  # parallel: GNU parallel (Perl)
        # parallel: https://github.com/mmstick/parallel (Rust)
        'rust-parallel': {'jobs': '-j', 'keep-order': ''},
        'gargs': {'jobs': '-p', 'keep-order': '-o'},  # gargs: https://github.com/brentp/gargs (Go)
        'rush': {'jobs': '-j', 'keep-order': '-k'}}  # rush: https://github.com/shenwei356/rush (Go)

njobs = 4

# Keep benchmark output away from existing files in the working directory.
output_dir = tempfile.TemporaryDirectory(prefix="rush-benchmark-")


def output_file(app):
    return os.path.join(output_dir.name, "t." + os.path.basename(app))


for test in tests:
    n = int((75 - len(test[0])) / 2)
    sys.stderr.write('{}[ {} ]{}\n'.format('=' * n,  test[0], '=' * n))

    for keep_order in [False, True]:
        msg = 'keep order: {}'.format(keep_order)
        n = int((70 - len(msg)) / 2)
        sys.stderr.write('{}[ {} ]{}\n'.format('-' * n,  msg, '-' * n))

        sys.stderr.write('\ncommands:\n')
        times = dict()
        for app, args in apps.items():
            cmd = '{} | {} {} {} {} "{}" > {}'.format(
                test[1], app, args['jobs'], njobs,
                args['keep-order'] if keep_order else '',
                test[2], shlex.quote(output_file(app)))

            sys.stderr.write('{}\n'.format(cmd))

            times[app] = timeit(cmd)

        sys.stderr.write('\ntime:\n')
        for app in sorted(times.keys()):
            sys.stderr.write('{}: {}\n'.format(app, times[app]))

        sys.stderr.write('\nmd5sum:\n')
        for app in sorted(times.keys()):
            subprocess.run(['md5sum', output_file(app)], check=True)
            os.remove(output_file(app))

        sys.stderr.write('\n')
    sys.stderr.write('\n')
