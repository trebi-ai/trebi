echo '{"type":"init","session_id":"sess-b"}'
read -r line
echo '{"type":"assistant","text":"frame turn 2"}'
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input":50,"output":5,"model":"default"}}'
