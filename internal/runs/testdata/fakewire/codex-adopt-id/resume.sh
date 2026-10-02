echo '{"type":"init","session_id":"thr-1"}'
read -r line
echo '{"type":"assistant","text":"frame turn 2"}'
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"thr-1","usage":{"input":50,"output":5,"model":"default"}}'
