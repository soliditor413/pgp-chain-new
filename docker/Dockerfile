FROM golang:1.26-alpine as bsc

RUN apk add --no-cache make cmake gcc musl-dev linux-headers git bash build-base libc-dev

ADD . /bsc
WORKDIR /bsc
ENV CGO_CFLAGS="-O -D__BLST_PORTABLE__" 
ENV CGO_CFLAGS_ALLOW="-O -D__BLST_PORTABLE__"
RUN make geth
RUN mv /bsc/build/bin/pgp /usr/local/bin/pgp

EXPOSE 8545 8547 30303 30303/udp
ENTRYPOINT [ "/usr/local/bin/pgp" ]

FROM ethereum/solc:0.6.4-alpine as bsc-genesis

RUN apk add --no-cache ca-certificates npm nodejs bash alpine-sdk expect

RUN git clone https://github.com/bnb-chain/bsc-genesis-contract.git /root/genesis \
    && cd /root/genesis && npm install
#RUN curl -L https://foundry.paradigm.xyz | bash

COPY --from=bsc /usr/local/bin/pgp /usr/local/bin/pgp

ENTRYPOINT [ "/bin/bash" ]
