fuser -k 8080/tcp 9090/tcp 2>/dev/null || true
rm -f /tmp/cloud-wasm-lab.pids
for CTX in kind-us-east kind-eu-north kind-ap-south; do
  kubectl --context "$CTX" delete pod follow-the-moon-wasm --ignore-not-found=true --wait=false 2>/dev/null || true
done
echo "Stopped services and cleaned up active Wasm pods."
