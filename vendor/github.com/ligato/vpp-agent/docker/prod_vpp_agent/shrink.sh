#!/bin/bash

set +e
sudo docker rmi -f prod_vpp_agent_shrink 
sudo docker rm -f shrink
set -e

sudo docker run -itd --name shrink prod_vpp_agent bash
sudo docker export shrink >shrink.tar
sudo docker rm -f shrink
sudo docker import -c "WORKDIR /root/" -c 'CMD ["/usr/bin/supervisord", "-c", "/etc/supervisord.conf"]' shrink.tar prod_vpp_agent_shrink
rm shrink.tar

