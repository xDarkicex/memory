package memory

import "sync/atomic"

// PIDController implements the cybernetic feedback loop for the HashMap.
// It actively tracks insertion velocity and collision lengths to dynamically
// scale the load-factor threshold. If the integral error spikes, it outputs a
// control variable to dictate the pace of incremental background rehashing.
type PIDController struct {
	kp float64 // Proportional gain
	ki float64 // Integral gain
	kd float64 // Derivative gain

	integral  float64
	prevError float64
	setpoint  float64

	// state is an atomically packed float64 representation to allow wait-free updates
	state atomic.Uint64
}

// NewPIDController initializes the cybernetic controller with baseline tunings.
func NewPIDController(kp, ki, kd, setpoint float64) *PIDController {
	return &PIDController{
		kp:       kp,
		ki:       ki,
		kd:       kd,
		setpoint: setpoint,
	}
}

// Update computes the PID control variable u(t) based on the current process variable.
func (pid *PIDController) Update(processVariable float64) float64 {
	errorVal := pid.setpoint - processVariable
	pid.integral += errorVal
	derivative := errorVal - pid.prevError

	output := (pid.kp * errorVal) + (pid.ki * pid.integral) + (pid.kd * derivative)
	pid.prevError = errorVal

	return output
}
