package holocropper

// PIDController is a discrete-time PID controller whose time step is supplied
// by the caller. In holocropper the time step is always 1/FPS (frame time),
// never wall-clock time, so controller behaviour is deterministic and
// reproducible run to run (HIGH-ASPECT-RATIO-FIXES.md F5).
//
// Step returns an adjustment (a velocity-like delta) that the caller applies
// to its own plant state (e.g. crop center or crop height). Actuator rate
// limits belong to the caller; the integral term is clamped internally as
// anti-windup.
type PIDController struct {
	Kp, Ki, Kd    float64
	integralLimit float64

	integral  float64
	lastError float64
	hasLast   bool
}

func NewPIDController(Kp, Ki, Kd, integralLimit float64) *PIDController {
	return &PIDController{
		Kp:            Kp,
		Ki:            Ki,
		Kd:            Kd,
		integralLimit: integralLimit,
	}
}

// Step advances the controller by dt seconds and returns the adjustment that
// moves `measured` toward `target`.
func (p *PIDController) Step(measured, target, dt float64) float64 {
	error := target - measured

	p.integral += error * dt
	p.integral = clampF(p.integral, -p.integralLimit, p.integralLimit)

	derivative := 0.0
	if p.hasLast {
		derivative = (error - p.lastError) / dt
	}

	out := p.Kp*error + p.Ki*p.integral + p.Kd*derivative

	p.lastError = error
	p.hasLast = true
	return out
}

// Reset clears integrator state, e.g. on a scene cut.
func (p *PIDController) Reset() {
	p.integral = 0
	p.lastError = 0
	p.hasLast = false
}

func clampF(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
