#!/bin/sh
set -e

VPP_CACHE_DIR=$HOME/build-cache/vpp
VPP_COMMIT="acd4c63e3c6e70ea3f58527d9bace7c0e38df719"

if [ ! -d "$VPP_CACHE_DIR" ]; then
    echo "Building VPP binaries."

    # build VPP
    git clone https://gerrit.fd.io/r/vpp 
    cd vpp 
    git checkout ${VPP_COMMIT} 
    yes | make install-dep     
    make bootstrap     
    make pkg-deb

    # copy deb packages to cache dir
    mkdir $VPP_CACHE_DIR
    cp build-root/*.deb $VPP_CACHE_DIR
else
    echo "Using cached VPP binaries from $VPP_CACHE_DIR"
fi

# install VPP
cd $VPP_CACHE_DIR
dpkg -i vpp_*.deb vpp-dev_*.deb vpp-lib_*.deb vpp-plugins_*.deb
