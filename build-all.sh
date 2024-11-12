export TAG=${1:-latest}
./build.sh $TAG x86_64
#./build.sh $TAG armv7l
#./build.sh $TAG aarch64
#./build.sh $TAG
./build.sh
