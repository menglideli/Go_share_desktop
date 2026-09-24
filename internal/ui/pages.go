// 本文件是各路由页面的布局与交互处理。
package ui

import (
	"fmt"
	"image"
	"image/color"
	"time"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	sysclip "goshare/internal/clip"
	"goshare/internal/capture"
)

// ---------------------------------------------------------------------------
// 事件处理
// ---------------------------------------------------------------------------

// handleEvents 消费当前路由下的控件事件。
//
// 只在当前路由下检查：未 Layout 的 Clickable 虽然不会收到事件（事件路由依赖
// ops 里声明的区域），但显式按路由分开能保证"上一页的残留点击"不会误触发。
func (s *Shell) handleEvents(gtx layout.Context) {
	switch s.Route() {
	case RouteHome:
		if s.btnShare.Clicked(gtx) {
			s.Go(RouteSetup)
		}
		if s.btnWatch.Clicked(gtx) {
			s.Go(RouteJoin)
		}

	case RouteSetup:
		for i := range s.cfg.Displays {
			if i < len(s.presetBtns) { // 占位，避免编译器优化掉循环变量
				_ = i
			}
		}
		if s.btnPick.Clicked(gtx) {
			if s.cfg.OnPickRegion != nil {
				snap := s.cfg.OnPickRegion(s.selDisplay)
				if snap == nil {
					s.Toast("抓取桌面快照失败")
				} else {
					s.SetSnapshot(snap)
					s.dragFrom = image.Point{}
					s.dragTo = image.Point{}
					s.dragging = false
					s.Go(RouteRegion)
				}
			}
		}
		if s.btnStart.Clicked(gtx) {
			if s.cfg.OnStartShare != nil {
				err := s.cfg.OnStartShare(ShareOptions{
					Display: s.selDisplay,
					Region:  s.region,
					Preset:  s.selPreset,
				})
				if err != nil {
					s.Toast("启动失败：" + err.Error())
				} else {
					s.Go(RouteSharing)
				}
			}
		}
		if s.btnBack.Clicked(gtx) {
			s.Go(RouteHome)
		}
		s.pickPreset(gtx)
		s.pickDisplay(gtx)

	case RouteSharing:
		if s.btnStop.Clicked(gtx) {
			if s.cfg.OnStopShare != nil {
				s.cfg.OnStopShare()
			}
			s.Go(RouteHome)
		}
		if s.btnPause.Clicked(gtx) && s.cfg.OnPause != nil {
			s.mu.Lock()
			paused := s.share.Paused
			s.mu.Unlock()
			s.cfg.OnPause(!paused)
		}
		if s.btnCodeCopy.Clicked(gtx) {
			s.mu.Lock()
			code := s.share.Code
			s.mu.Unlock()
			s.copy(code, "授权码已复制")
		}
		if s.btnRotate.Clicked(gtx) && s.cfg.OnRotate != nil {
			s.Toast("授权码已轮换")
			_ = s.cfg.OnRotate()
		}
		s.pickAddrCopy(gtx)
		s.pickKick(gtx)
		s.pickPreset(gtx)

	case RouteJoin:
		if s.btnJoin.Clicked(gtx) && s.cfg.OnJoin != nil {
			addr := s.edAddr.Text()
			code := s.edCode.Text()
			if addr == "" || code == "" {
				s.Toast("请填写地址和授权码")
			} else {
				s.cfg.OnJoin(addr, code)
			}
		}
		if s.btnScan.Clicked(gtx) && s.cfg.OnScan != nil {
			s.cfg.OnScan()
		}
		if s.btnBack.Clicked(gtx) {
			s.Go(RouteHome)
		}
		s.pickFound(gtx)

	case RouteViewing:
		if s.btnLeave.Clicked(gtx) {
			if s.cfg.OnLeaveView != nil {
				s.cfg.OnLeaveView()
			}
			s.Go(RouteHome)
		}

	case RouteRegion:
		if s.btnRegionOK.Clicked(gtx) {
			if s.cfg.OnRegionDone != nil {
				s.cfg.OnRegionDone(s.region, s.region.W > 8 && s.region.H > 8)
			}
			s.Go(RouteSetup)
		}
		if s.btnRegionNo.Clicked(gtx) {
			if s.cfg.OnRegionDone != nil {
				s.cfg.OnRegionDone(capture.Rect{}, false)
			}
			s.Go(RouteSetup)
		}
	}
}

// copy 复制到剪贴板并提示。
func (s *Shell) copy(txt, okMsg string) {
	if txt == "" {
		return
	}
	if err := sysclip.SetText(txt); err != nil {
		s.Toast("复制失败：" + err.Error())
		return
	}
	s.Toast(okMsg)
}

func (s *Shell) pickAddrCopy(gtx layout.Context) {
	s.mu.Lock()
	addrs := s.share.Addrs
	s.mu.Unlock()
	for i := range s.addrCopies {
		if i < len(addrs) && s.addrCopies[i].Clicked(gtx) {
			s.copy(addrs[i], "地址已复制")
		}
	}
}

func (s *Shell) pickKick(gtx layout.Context) {
	s.mu.Lock()
	vs := s.share.Viewers
	s.mu.Unlock()
	for i := range s.kickBtns {
		if i < len(vs) && s.kickBtns[i].Clicked(gtx) && s.cfg.OnKick != nil {
			s.cfg.OnKick(vs[i].IP)
		}
	}
}

func (s *Shell) pickFound(gtx layout.Context) {
	s.mu.Lock()
	found := s.join.Found
	s.mu.Unlock()
	for i := range s.foundBtns {
		if i < len(found) && s.foundBtns[i].Clicked(gtx) {
			s.edAddr.SetText(found[i].Addr)
			s.Toast("已填入 " + found[i].Name)
		}
	}
}

// presetHighlight 返回当前应高亮的档位名。
//
// 分享中必须以**实际生效的档位**为准（业务侧回传的 PresetName），
// 而不是界面本地点了什么 —— 两者不一致时用户会以为改成功了，其实没改。
func (s *Shell) presetHighlight() string {
	if s.Route() == RouteSharing {
		s.mu.Lock()
		n := s.share.PresetName
		s.mu.Unlock()
		if n != "" {
			return n
		}
	}
	return s.selPreset
}

// pickPreset 处理质量档位按钮。
func (s *Shell) pickPreset(gtx layout.Context) {
	for i := range s.presetBtns {
		if s.presetBtns[i].Clicked(gtx) && i < len(s.cfg.Presets) {
			name := s.cfg.Presets[i]
			s.selPreset = name
			if s.cfg.OnPreset != nil && s.Route() == RouteSharing {
				s.cfg.OnPreset(name)
			}
		}
	}
}

// pickDisplay 处理显示器选择（用 Enum 的 radio）。
func (s *Shell) pickDisplay(gtx layout.Context) {
	_ = gtx
	if k := s.dispEnum.Value; k != "" {
		for _, d := range s.cfg.Displays {
			if fmt.Sprintf("%d", d.ID) == k {
				s.selDisplay = d
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 首页
// ---------------------------------------------------------------------------

func (s *Shell) pageHome(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical, Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.H3(th, "GoShare")
				l.Color = colText
				return l.Layout(gtx)
			}),
			layout.Rigid(vSpace(6)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Body1(th, "内网桌面共享 · 低延迟 · 无需服务器")
				l.Color = colDim
				return l.Layout(gtx)
			}),
			layout.Rigid(vSpace(40)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				b := material.Button(th, &s.btnShare, "我要分享")
				b.Background = colAccent
				b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
				b.CornerRadius = unit.Dp(10)
				b.TextSize = unit.Sp(20)
				b.Inset = layout.UniformInset(unit.Dp(20))
				gtx.Constraints.Min.X = gtx.Dp(unit.Dp(320))
				return b.Layout(gtx)
			}),
			layout.Rigid(vSpace(18)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				b := material.Button(th, &s.btnWatch, "我要观看")
				b.Background = colPanel
				b.Color = colText
				b.CornerRadius = unit.Dp(10)
				b.TextSize = unit.Sp(20)
				b.Inset = layout.UniformInset(unit.Dp(20))
				gtx.Constraints.Min.X = gtx.Dp(unit.Dp(320))
				return b.Layout(gtx)
			}),
			layout.Rigid(vSpace(40)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.Caption(th, fmt.Sprintf("已识别 %d 台显示器", len(s.cfg.Displays)))
				l.Color = colDim
				return l.Layout(gtx)
			}),
		)
	})
}

// ---------------------------------------------------------------------------
// 分享设置页
// ---------------------------------------------------------------------------

func (s *Shell) pageSetup(gtx layout.Context, th *material.Theme) layout.Dimensions {
	return layout.UniformInset(unit.Dp(24)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				l := material.H5(th, "分享设置")
				l.Color = colText
				return l.Layout(gtx)
			}),
			layout.Rigid(vSpace(18)),
			// 显示器选择
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.section(gtx, th, "要分享的画面", func(gtx layout.Context) layout.Dimensions {
					children := make([]layout.FlexChild, 0, len(s.cfg.Displays))
					for _, d := range s.cfg.Displays {
						d := d
						txt := fmt.Sprintf("%s  %d×%d", d.Name, d.W, d.H)
						if d.Primary {
							txt += "  （主屏）"
						}
						key := fmt.Sprintf("%d", d.ID)
						if s.dispEnum.Value == "" && d.ID == s.selDisplay.ID {
							s.dispEnum.Value = key
						}
						children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							rb := material.RadioButton(th, &s.dispEnum, key, txt)
							rb.Color = colText
							return rb.Layout(gtx)
						}))
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
				})
			}),
			layout.Rigid(vSpace(14)),
			// 区域
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.section(gtx, th, "共享区域", func(gtx layout.Context) layout.Dimensions {
					txt := "整屏"
					if !s.region.Empty() {
						txt = fmt.Sprintf("自定义 %d×%d @ (%d,%d)", s.region.W, s.region.H, s.region.X, s.region.Y)
					}
					return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							l := material.Body1(th, txt)
							l.Color = colText
							return l.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							b := material.Button(th, &s.btnPick, "框选区域")
							b.Background = colPanel
							b.Color = colText
							b.CornerRadius = unit.Dp(8)
							return b.Layout(gtx)
						}),
					)
				})
			}),
			layout.Rigid(vSpace(14)),
			// 质量
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return s.section(gtx, th, "画质与帧率", func(gtx layout.Context) layout.Dimensions {
					return s.presetRow(gtx, th)
				})
			}),
			layout.Rigid(vSpace(14)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				s.mu.Lock()
				note := s.setupNote
				s.mu.Unlock()
				if note == "" {
					return layout.Dimensions{}
				}
				l := material.Body2(th, note)
				l.Color = colWarn
				return l.Layout(gtx)
			}),
			layout.Rigid(vSpace(20)),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th, &s.btnBack, "返回")
						b.Background = colPanel
						b.Color = colText
						b.CornerRadius = unit.Dp(8)
						return b.Layout(gtx)
					}),
					layout.Rigid(hSpace(12)),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th, &s.btnStart, "开始分享")
						b.Background = colAccent
						b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
						b.CornerRadius = unit.Dp(8)
						return b.Layout(gtx)
					}),
				)
			}),
		)
	})
}

// presetRow 画质量档位按钮行，当前档位高亮。
func (s *Shell) presetRow(gtx layout.Context, th *material.Theme) layout.Dimensions {
	for len(s.presetBtns) < len(s.cfg.Presets) {
		s.presetBtns = append(s.presetBtns, widget.Clickable{})
	}
	children := make([]layout.FlexChild, 0, len(s.cfg.Presets)+2)
	hl := s.presetHighlight()
	for i, name := range s.cfg.Presets {
		i, name := i, name
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			sel := name == hl
			bg := colPanel
			fg := colText
			if sel {
				bg = colAccent
				fg = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
			}
			b := material.Button(th, &s.presetBtns[i], name)
			b.Background = bg
			b.Color = fg
			b.CornerRadius = unit.Dp(8)
			return b.Layout(gtx)
		}))
		children = append(children, layout.Rigid(hSpace(8)))
	}
	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, children...)
}

// section 画一个带标题的卡片区块。
func (s *Shell) section(gtx layout.Context, th *material.Theme, title string, body layout.Widget) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(th, title)
			l.Color = colDim
			return l.Layout(gtx)
		}),
		layout.Rigid(vSpace(6)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			// 卡片背景：先量内容高度，再按高度裁剪着画（R24 的教训）
			m := op.Record(gtx.Ops)
			gtx.Constraints.Min = image.Point{}
			inner := layout.UniformInset(unit.Dp(14)).Layout(gtx, body)
			c := m.Stop()
			paint.FillShape(gtx.Ops, colPanel,
				clip.UniformRRect(image.Rectangle{Max: image.Pt(gtx.Constraints.Max.X, inner.Size.Y)}, 10).Op(gtx.Ops))
			c.Add(gtx.Ops)
			return inner
		}),
	)
}

// ---------------------------------------------------------------------------
// 分享中
// ---------------------------------------------------------------------------

func (s *Shell) pageSharing(gtx layout.Context, th *material.Theme) layout.Dimensions {
	s.mu.Lock()
	st := s.share
	prev := s.preview
	s.mu.Unlock()

	return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
		// 左：本地回显
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				rect := clip.Rect(image.Rectangle{Max: gtx.Constraints.Max})
				stack := rect.Push(gtx.Ops)
				paint.Fill(gtx.Ops, color.NRGBA{R: 8, G: 11, B: 14, A: 255})
				if prev != nil && !prev.Bounds().Empty() {
					widget.Image{
						Src:      paint.NewImageOp(prev),
						Fit:      widget.Contain,
						Position: layout.Center,
						Scale:    1 / gtx.Metric.PxPerDp,
					}.Layout(gtx)
				} else {
					layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Body1(th, "正在建立画面…")
						l.Color = colDim
						return l.Layout(gtx)
					})
				}
				if st.Paused {
					// 暂停遮罩：明确告诉用户"画面没推给观众"
					paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 170})
					layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.H5(th, "已暂停分享")
						l.Color = colWarn
						return l.Layout(gtx)
					})
				}
				stack.Pop()
				return layout.Dimensions{Size: gtx.Constraints.Max}
			})
		}),
		// 右：控制面板（固定宽度）
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = gtx.Dp(unit.Dp(360))
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.UniformInset(unit.Dp(16)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					// 授权码
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.section(gtx, th, "授权码", func(gtx layout.Context) layout.Dimensions {
							return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
								layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
									l := material.H4(th, st.Code)
									l.Color = colGood
									return l.Layout(gtx)
								}),
								layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									b := material.Button(th, &s.btnCodeCopy, "复制")
									b.Background = colAccent
									b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
									b.CornerRadius = unit.Dp(8)
									return b.Layout(gtx)
								}),
							)
						})
					}),
					layout.Rigid(vSpace(12)),
					// 地址列表
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.section(gtx, th, "连接地址（同事用其中一个）", func(gtx layout.Context) layout.Dimensions {
							children := make([]layout.FlexChild, 0, len(st.Addrs)*2)
							for i, a := range st.Addrs {
								i, a := i, a
								children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											l := material.Body2(th, a)
											l.Color = colText
											return l.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											if i >= len(s.addrCopies) {
												return layout.Dimensions{}
											}
											b := material.Button(th, &s.addrCopies[i], "复制")
											b.Background = colBG
											b.Color = colText
											b.CornerRadius = unit.Dp(6)
											b.TextSize = unit.Sp(12)
											b.Inset = layout.UniformInset(unit.Dp(4))
											return b.Layout(gtx)
										}),
									)
								}))
								children = append(children, layout.Rigid(vSpace(6)))
							}
							if len(children) == 0 {
								l := material.Body2(th, "（无可用地址）")
								l.Color = colDim
								return l.Layout(gtx)
							}
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
						})
					}),
					layout.Rigid(vSpace(12)),
					// HUD
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.section(gtx, th, "运行状态", func(gtx layout.Context) layout.Dimensions {
							l := material.Body2(th, st.HUD)
							l.Color = colText
							return l.Layout(gtx)
						})
					}),
					layout.Rigid(vSpace(12)),
					// 质量
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.section(gtx, th, "画质与帧率", func(gtx layout.Context) layout.Dimensions {
							return s.presetRow(gtx, th)
						})
					}),
					layout.Rigid(vSpace(12)),
					// 观众列表
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						return s.section(gtx, th, fmt.Sprintf("观众（%d 人）", len(st.Viewers)), func(gtx layout.Context) layout.Dimensions {
							if len(st.Viewers) == 0 {
								l := material.Body2(th, "暂无观众")
								l.Color = colDim
								return l.Layout(gtx)
							}
							children := make([]layout.FlexChild, 0, len(st.Viewers)*2)
							for i, v := range st.Viewers {
								i, v := i, v
								children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
										layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
											d := time.Since(v.Since).Truncate(time.Second)
											l := material.Body2(th, fmt.Sprintf("%s · %v", v.Name, d))
											l.Color = colText
											return l.Layout(gtx)
										}),
										layout.Rigid(func(gtx layout.Context) layout.Dimensions {
											if i >= len(s.kickBtns) {
												return layout.Dimensions{}
											}
											b := material.Button(th, &s.kickBtns[i], "断开")
											b.Background = colBG
											b.Color = colWarn
											b.CornerRadius = unit.Dp(6)
											b.TextSize = unit.Sp(12)
											b.Inset = layout.UniformInset(unit.Dp(4))
											return b.Layout(gtx)
										}),
									)
								}))
								children = append(children, layout.Rigid(vSpace(6)))
							}
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
						})
					}),
					layout.Rigid(vSpace(16)),
					// 操作
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						pauseTxt := "暂停分享"
						if st.Paused {
							pauseTxt = "继续分享"
						}
						return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								gtx.Constraints.Min.X = gtx.Constraints.Max.X
								b := material.Button(th, &s.btnPause, pauseTxt)
								b.Background = colPanel
								b.Color = colText
								b.CornerRadius = unit.Dp(8)
								return b.Layout(gtx)
							}),
							layout.Rigid(vSpace(8)),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								gtx.Constraints.Min.X = gtx.Constraints.Max.X
								b := material.Button(th, &s.btnRotate, "轮换授权码")
								b.Background = colPanel
								b.Color = colText
								b.CornerRadius = unit.Dp(8)
								return b.Layout(gtx)
							}),
							layout.Rigid(vSpace(8)),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								gtx.Constraints.Min.X = gtx.Constraints.Max.X
								b := material.Button(th, &s.btnStop, "停止分享")
								b.Background = colWarn
								b.Color = color.NRGBA{R: 24, G: 18, B: 4, A: 255}
								b.CornerRadius = unit.Dp(8)
								return b.Layout(gtx)
							}),
						)
					}),
				)
			})
		}),
	)
}

// ---------------------------------------------------------------------------
// 观看接入页
// ---------------------------------------------------------------------------

func (s *Shell) pageJoin(gtx layout.Context, th *material.Theme) layout.Dimensions {
	s.mu.Lock()
	js := s.join
	s.mu.Unlock()

	return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		gtx.Constraints.Max.X = min(gtx.Constraints.Max.X, gtx.Dp(unit.Dp(620)))
		return layout.UniformInset(unit.Dp(24)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					l := material.H5(th, "观看他人的屏幕")
					l.Color = colText
					return l.Layout(gtx)
				}),
				layout.Rigid(vSpace(18)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, th, "地址（IP:端口）", "192.168.1.10:9000", &s.edAddr)
				}),
				layout.Rigid(vSpace(12)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return s.field(gtx, th, "授权码", "6 位数字", &s.edCode)
				}),
				layout.Rigid(vSpace(16)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							b := material.Button(th, &s.btnJoin, "连接")
							b.Background = colAccent
							b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
							b.CornerRadius = unit.Dp(8)
							return b.Layout(gtx)
						}),
						layout.Rigid(hSpace(10)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							txt := "扫描局域网"
							if js.Scanning {
								txt = "扫描中…"
							}
							b := material.Button(th, &s.btnScan, txt)
							b.Background = colPanel
							b.Color = colText
							b.CornerRadius = unit.Dp(8)
							return b.Layout(gtx)
						}),
						layout.Rigid(hSpace(10)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							b := material.Button(th, &s.btnBack, "返回")
							b.Background = colPanel
							b.Color = colText
							b.CornerRadius = unit.Dp(8)
							return b.Layout(gtx)
						}),
					)
				}),
				layout.Rigid(vSpace(16)),
				// 状态
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if js.Status == "" && js.Err == "" {
						return layout.Dimensions{}
					}
					txt := js.Status
					c := colText
					if js.Err != "" {
						txt = js.Err
						c = colWarn
					}
					l := material.Body2(th, txt)
					l.Color = c
					return l.Layout(gtx)
				}),
				// 发现列表
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					if len(js.Found) == 0 {
						return layout.Dimensions{}
					}
					return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
						layout.Rigid(vSpace(14)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							l := material.Body2(th, "局域网里的分享")
							l.Color = colDim
							return l.Layout(gtx)
						}),
						layout.Rigid(vSpace(6)),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							children := make([]layout.FlexChild, 0, len(js.Found)*2)
							for i, f := range js.Found {
								i, f := i, f
								children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
									if i >= len(s.foundBtns) {
										return layout.Dimensions{}
									}
									gtx.Constraints.Min.X = gtx.Constraints.Max.X
									b := material.Button(th, &s.foundBtns[i], f.Name+"  "+f.Addr)
									b.Background = colPanel
									b.Color = colText
									b.CornerRadius = unit.Dp(8)
									return b.Layout(gtx)
								}))
								children = append(children, layout.Rigid(vSpace(6)))
							}
							return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
						}),
					)
				}),
			)
		})
	})
}

// field 画一个带标签的输入框。
func (s *Shell) field(gtx layout.Context, th *material.Theme, label, hint string, ed *widget.Editor) layout.Dimensions {
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			l := material.Body2(th, label)
			l.Color = colDim
			return l.Layout(gtx)
		}),
		layout.Rigid(vSpace(4)),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			m := op.Record(gtx.Ops)
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			inner := layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				e := material.Editor(th, ed, hint)
				e.Color = colText
				e.HintColor = colDim
				return e.Layout(gtx)
			})
			c := m.Stop()
			paint.FillShape(gtx.Ops, colPanel,
				clip.UniformRRect(image.Rectangle{Max: inner.Size}, 8).Op(gtx.Ops))
			c.Add(gtx.Ops)
			return inner
		}),
	)
}

// ---------------------------------------------------------------------------
// 观看中
// ---------------------------------------------------------------------------

func (s *Shell) pageViewing(gtx layout.Context, th *material.Theme) layout.Dimensions {
	s.mu.Lock()
	img := s.lastFrame
	js := s.join
	s.mu.Unlock()

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		// 顶部条
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			m := op.Record(gtx.Ops)
			gtx.Constraints.Min = image.Point{}
			inner := layout.UniformInset(unit.Dp(10)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						txt := js.Status
						if txt == "" {
							txt = "观看中"
						}
						l := material.Body2(th, txt)
						l.Color = colText
						return l.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th, &s.btnLeave, "退出观看")
						b.Background = colPanel
						b.Color = colText
						b.CornerRadius = unit.Dp(8)
						b.TextSize = unit.Sp(12)
						b.Inset = layout.UniformInset(unit.Dp(6))
						return b.Layout(gtx)
					}),
				)
			})
			c := m.Stop()
			paint.FillShape(gtx.Ops, colPanel,
				clip.Rect(image.Rectangle{Max: image.Pt(gtx.Constraints.Max.X, inner.Size.Y)}).Op())
			c.Add(gtx.Ops)
			return inner
		}),
		// 画面
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			stack := clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Push(gtx.Ops)
			paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
			if img != nil && !img.Bounds().Empty() {
				widget.Image{
					Src:      paint.NewImageOp(img),
					Fit:      widget.Contain,
					Position: layout.Center,
					Scale:    1 / gtx.Metric.PxPerDp,
				}.Layout(gtx)
			} else {
				layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					l := material.Body1(th, "等待画面…")
					l.Color = colDim
					return l.Layout(gtx)
				})
			}
			stack.Pop()
			return layout.Dimensions{Size: gtx.Constraints.Max}
		}),
	)
}

// ---------------------------------------------------------------------------
// 区域框选
// ---------------------------------------------------------------------------

func (s *Shell) pageRegion(gtx layout.Context, th *material.Theme) layout.Dimensions {
	s.mu.Lock()
	snap := s.snapshot
	s.mu.Unlock()

	win := gtx.Constraints.Max
	// 快照以 contain 方式铺满窗口；先算出它在窗口里的实际矩形，
	// 拖拽坐标才能准确映射回桌面像素。
	var dst image.Rectangle
	if snap != nil && !snap.Bounds().Empty() {
		dst = containRect(win, image.Pt(snap.Bounds().Dx(), snap.Bounds().Dy()))
	}

	// 拖拽输入：整块区域都接收按下/拖动/抬起
	area := clip.Rect(image.Rectangle{Max: win}).Push(gtx.Ops)
	event.Op(gtx.Ops, &s.dragTag)
	tag := &s.dragTag
	for {
		ev, ok := gtx.Event(pointer.Filter{
			Target: tag,
			Kinds:  pointer.Press | pointer.Drag | pointer.Release | pointer.Cancel,
		})
		if !ok {
			break
		}
		e, ok := ev.(pointer.Event)
		if !ok {
			continue
		}
		p := image.Pt(int(e.Position.X*gtx.Metric.PxPerDp), int(e.Position.Y*gtx.Metric.PxPerDp))
		switch e.Kind {
		case pointer.Press:
			s.dragging = true
			s.dragFrom = p
			s.dragTo = p
		case pointer.Drag, pointer.Move:
			if s.dragging {
				s.dragTo = p
			}
		case pointer.Release:
			if s.dragging {
				s.dragTo = p
				s.dragging = false
			}
		case pointer.Cancel:
			s.dragging = false
		}
	}

	// 画快照
	if snap != nil && !snap.Bounds().Empty() {
		dstStack := clip.Rect(dst).Push(gtx.Ops)
		widget.Image{
			Src:      paint.NewImageOp(snap),
			Fit:      widget.Contain,
			Position: layout.Center,
			Scale:    1 / gtx.Metric.PxPerDp,
		}.Layout(gtx)
		dstStack.Pop()
	}

	// 把拖拽框换算成窗口矩形
	winRect := normRect(s.dragFrom, s.dragTo)
	sel := winRect.Intersect(dst)
	if sel.Empty() {
		// 还没选：整块压暗
		paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 120})
	} else {
		// 挖洞：四周四块遮罩
		mask := func(r image.Rectangle) {
			if r.Empty() {
				return
			}
			st := clip.Rect(r).Push(gtx.Ops)
			paint.Fill(gtx.Ops, color.NRGBA{R: 0, G: 0, B: 0, A: 120})
			st.Pop()
		}
		mask(image.Rect(dst.Min.X, dst.Min.Y, dst.Max.X, sel.Min.Y))
		mask(image.Rect(dst.Min.X, sel.Max.Y, dst.Max.X, dst.Max.Y))
		mask(image.Rect(dst.Min.X, sel.Min.Y, sel.Min.X, sel.Max.Y))
		mask(image.Rect(sel.Max.X, sel.Min.Y, dst.Max.X, sel.Max.Y))
		// 边框
		border := func(r image.Rectangle) {
			if r.Empty() {
				return
			}
			st := clip.Rect(r).Push(gtx.Ops)
			paint.Fill(gtx.Ops, colAccent)
			st.Pop()
		}
		b := 2
		border(image.Rect(sel.Min.X, sel.Min.Y, sel.Max.X, sel.Min.Y+b))
		border(image.Rect(sel.Min.X, sel.Max.Y-b, sel.Max.X, sel.Max.Y))
		border(image.Rect(sel.Min.X, sel.Min.Y, sel.Min.X+b, sel.Max.Y))
		border(image.Rect(sel.Max.X-b, sel.Min.Y, sel.Max.X, sel.Max.Y))
	}

	// 换算成桌面像素坐标存起来（确认时用）
	if snap != nil && !snap.Bounds().Empty() && !sel.Empty() {
		iw := snap.Bounds().Dx()
		sc := float64(dst.Dx()) / float64(iw)
		if sc <= 0 {
			sc = 1
		}
		x := int(float64(sel.Min.X-dst.Min.X) / sc)
		y := int(float64(sel.Min.Y-dst.Min.Y) / sc)
		w := int(float64(sel.Dx()) / sc)
		h := int(float64(sel.Dy()) / sc)
		s.region = capture.Rect{X: x, Y: y, W: w, H: h}.Clamp(s.selDisplay)
	}
	area.Pop()

	// 底部操作条
	return layout.S.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.UniformInset(unit.Dp(20)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					txt := "按住鼠标左键拖出要共享的区域"
					if !s.region.Empty() {
						txt = fmt.Sprintf("已选 %d×%d @ (%d,%d)", s.region.W, s.region.H, s.region.X, s.region.Y)
					}
					return s.chip(gtx, th, txt, colPanel)
				}),
				layout.Rigid(hSpace(12)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					b := material.Button(th, &s.btnRegionNo, "取消")
					b.Background = colPanel
					b.Color = colText
					b.CornerRadius = unit.Dp(8)
					return b.Layout(gtx)
				}),
				layout.Rigid(hSpace(8)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					b := material.Button(th, &s.btnRegionOK, "使用这个区域")
					b.Background = colAccent
					b.Color = color.NRGBA{R: 255, G: 255, B: 255, A: 255}
					b.CornerRadius = unit.Dp(8)
					return b.Layout(gtx)
				}),
			)
		})
	})
}

// containRect 计算图像以 contain 方式放进 win 后的实际矩形（与 widget.Fit=Contain 一致）。
func containRect(win, img image.Point) image.Rectangle {
	if img.X <= 0 || img.Y <= 0 || win.X <= 0 || win.Y <= 0 {
		return image.Rectangle{Max: win}
	}
	s := float64(win.X) / float64(img.X)
	if sy := float64(win.Y) / float64(img.Y); sy < s {
		s = sy
	}
	w := int(float64(img.X) * s)
	h := int(float64(img.Y) * s)
	ox := (win.X - w) / 2
	oy := (win.Y - h) / 2
	return image.Rect(ox, oy, ox+w, oy+h)
}

func normRect(a, b image.Point) image.Rectangle {
	x0, x1 := a.X, b.X
	if x1 < x0 {
		x0, x1 = x1, x0
	}
	y0, y1 := a.Y, b.Y
	if y1 < y0 {
		y0, y1 = y1, y0
	}
	return image.Rect(x0, y0, x1, y1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// vSpace / hSpace 生成间距。
func vSpace(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Dimensions{Size: image.Pt(0, gtx.Dp(unit.Dp(dp)))}
	}
}

func hSpace(dp int) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Dimensions{Size: image.Pt(gtx.Dp(unit.Dp(dp)), 0)}
	}
}

// 保证 f32 被用到（pointer.Event.Position 是 f32.Point，这里保留导入以明确依赖）
var _ = f32.Point{}
