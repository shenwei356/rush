#!/usr/bin/env sh

# https://github.com/mitchellh/gox
CGO_ENABLED=0 gox -os="windows darwin linux freebsd" -arch="386 amd64 arm64" \
    -tags netgo -ldflags "-extldflags '-static' -w -s $commit" -asmflags '-trimpath'

dir=binaries
mkdir -p $dir;
rm -rf $dir/$f;

for f in rush_*; do
    mkdir -p $dir/$f;
    mv $f $dir/$f;
    cd $dir/$f;
    mv $f $(echo $f | perl -pe 's/_[^\.]+//g');
    tar -zcf $f.tar.gz rush*;
    mv *.tar.gz ../;
    cd ..;
    rm -rf $f;
    cd ..;
done;

ls binaries/*.tar.gz | rush 'cd {/}; md5sum {%} > {%}.md5.txt'
