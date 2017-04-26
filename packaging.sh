#!/usr/bin/env sh

# https://github.com/mitchellh/gox
gox -os="windows darwin linux" -arch="386 amd64";

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
