package workpool

// Step exposes the controller's single sampling and scaling decision so tests
// can drive it deterministically without real time.
func (c *Controller) Step() {
	c.step()
}
