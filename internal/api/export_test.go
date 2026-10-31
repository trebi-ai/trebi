package api

import "github.com/trebi-ai/trebi/internal/moves"

// SetMoves rebuilds the move service with test options. Call it before the
// first request.
func (s *Server) SetMoves(o moves.Options) {
	if o.Machines == nil {
		o.Machines = s.machines
	}
	s.moves.Close()
	s.moves = moves.New(s.st, s.ws, o)
}
