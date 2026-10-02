echo '{"type":"init","session_id":"sess-a"}'
read -r line
echo '{"type":"assistant","text":"frame turn 1"}'
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"usage":{"input":100,"output":10,"model":"default"}}'
