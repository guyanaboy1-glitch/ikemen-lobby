--[[
  Ikemen Party - Online Lobby mod for Ikemen GO
  ==============================================
  Adds ONLINE LOBBY to the title menu. From there you can:
    - browse live sessions from everyone using the Online Lobby
    - join NETPLAY sessions right here in Ikemen (same version required)
    - watch / play STREAM sessions (opens in your web browser)
    - host your own stream or netplay session

  Ikemen's Lua can't use the network, so this mod talks to IkemenParty.exe
  (placed next to Ikemen_GO.exe) through two small files in save/party/.
  The mod starts IkemenParty.exe in the background when needed.
]]

local P = {}
P.VERSION = '0.3.0'
P.dir = 'save/party/'
P.statusFile = P.dir .. 'party_status.json'
P.cmdFile = P.dir .. 'party_cmd.json'
P.exe = 'IkemenParty.exe'
P.isWindows = os.getenv('OS') == 'Windows_NT'
P.status = nil
P.seq = 0
P.pending = nil
P.launchedAt = nil
P.netplayInfoShown = false

-------------------------------------------------------------------------------
-- Talking to Ikemen Party
-------------------------------------------------------------------------------

function P.readStatus()
	if not fileExists(P.statusFile) then
		return P.status
	end
	-- jsonDecode raises on a half-written file; just keep the last good copy.
	local ok, st = pcall(jsonDecode, P.statusFile)
	if ok and type(st) == 'table' then
		P.status = st
	end
	return P.status
end

function P.alive()
	local st = P.status
	return st ~= nil and type(st.alive) == 'number' and st.alive >= os.time() - 5
end

function P.send(cmd, args)
	P.seq = P.seq + 1
	local t = {}
	for k, v in pairs(args or {}) do t[k] = v end
	t.cmd = cmd
	t.id = os.time() * 1000 + (P.seq % 1000)
	pcall(jsonEncode, t, P.cmdFile)
	P.pending = {id = t.id, cmd = cmd, args = args, at = os.time()}
	return t.id
end

function P.acked(id)
	return P.status ~= nil and P.status.last_cmd == id
end

-- Starts IkemenParty.exe in the background if it isn't running.
function P.launch()
	if P.launchedAt ~= nil and os.time() - P.launchedAt < 20 then
		return
	end
	P.launchedAt = os.time()
	if not fileExists(P.exe) then
		P.launchError = P.exe .. ' not found next to Ikemen_GO.exe.'
		return
	end
	P.launchError = nil
	if P.isWindows then
		-- No quotes on purpose: cmd /c + quoting is fragile.
		os.execute('start /min ' .. P.exe .. ' --companion')
	else
		P.launchError = 'Start IkemenParty yourself on this system.'
	end
end

-- Called a few times per second while a lobby screen is open.
function P.tick()
	P.readStatus()
	if not P.alive() then
		P.launch()
		return
	end
	local p = P.pending
	if p ~= nil and not P.acked(p.id) and os.time() - p.at >= 3 then
		P.send(p.cmd, p.args) -- not picked up (e.g. app was starting): resend
	elseif p == nil or P.acked(p.id) then
		if P.lastPing == nil or os.time() - P.lastPing >= 3 then
			P.lastPing = os.time()
			P.send('ping')
		end
	end
end

-- Sends a command and waits (drawing a message) until the app picks it up.
function P.sendAndWait(cmd, args, text, timeout, until_fn)
	local id = P.send(cmd, args)
	local start = os.time()
	while os.time() - start < (timeout or 10) do
		P.readStatus()
		if P.acked(id) and (until_fn == nil or until_fn(P.status)) then
			return true
		end
		if not P.alive() then
			P.launch()
		elseif not P.acked(id) and os.time() - P.pending.at >= 3 then
			id = P.send(cmd, args)
		end
		if esc() or getInput(-1, motif[main.group].menu.cancel.key) then
			sndPlay(motif.Snd, motif[main.group].cancel.snd[1], motif[main.group].cancel.snd[2])
			return false, 'cancelled'
		end
		P.drawBusy(text)
	end
	return false, 'Ikemen Party did not answer in time.'
end

-------------------------------------------------------------------------------
-- Drawing
-------------------------------------------------------------------------------

local W, H = 320, 240
local pool, poolN, rects, rectN = {}, 0, {}, 0
local font = nil
local frame = 0

local function txt(s, x, y, r, g, b, align)
	poolN = poolN + 1
	local ts = pool[poolN]
	if ts == nil then
		ts = textImgNew()
		pool[poolN] = ts
	end
	if font == nil then
		font = fontNew('f-6x9.def')
	end
	textImgSetFont(ts, font)
	textImgSetLocalcoord(ts, W, H)
	textImgSetBank(ts, 0)
	textImgSetAlign(ts, align or 1)
	textImgSetColor(ts, r or 255, g or 255, b or 255)
	textImgSetScale(ts, 1, 1)
	textImgSetText(ts, s)
	textImgSetPos(ts, x, y)
	textImgDraw(ts)
end

local function box(x1, y1, x2, y2, r, g, b, a)
	rectN = rectN + 1
	local rc = rects[rectN]
	if rc == nil then
		rc = rectNew()
		rects[rectN] = rc
	end
	rectSetLocalcoord(rc, W, H)
	rectSetWindow(rc, x1, y1, x2, y2)
	rectSetColor(rc, r, g, b)
	rectSetAlpha(rc, a or 255, 255 - (a or 255))
	rectDraw(rc)
end

local function clip(s, n)
	s = tostring(s or '')
	if #s > n then
		return s:sub(1, n - 2) .. '..'
	end
	return s
end

local function beginFrame()
	frame = frame + 1
	poolN, rectN = 0, 0
	clearColor(motif[main.background].bgclearcolor[1], motif[main.background].bgclearcolor[2], motif[main.background].bgclearcolor[3])
	bgDraw(motif[main.background].BGDef, 0)
	box(0, 0, W, H, 4, 8, 20, 190)
end

local function endFrame()
	bgDraw(motif[main.background].BGDef, 1)
	refresh()
end

local CYAN = {76, 195, 255}
local GOLD = {230, 184, 76}
local GREEN = {68, 211, 154}
local RED = {255, 95, 109}
local GREY = {141, 153, 179}
local WHITE = {230, 235, 245}

local function txtc(s, x, y, c, align)
	txt(s, x, y, c[1], c[2], c[3], align)
end

local function header(sub)
	txtc('ONLINE LOBBY', W / 2, 20, CYAN, 0)
	if sub ~= nil then
		txtc(clip(sub, 52), W / 2, 32, GREY, 0)
	end
end

function P.drawBusy(text)
	beginFrame()
	header()
	local dots = string.rep('.', math.floor(frame / 20) % 4)
	txtc(text .. dots, W / 2, 116, WHITE, 0)
	txtc('Cancel: back', W / 2, 226, GREY, 0)
	endFrame()
end

-------------------------------------------------------------------------------
-- Menus
-------------------------------------------------------------------------------

local function keyNext() return getInput(-1, motif[main.group].menu.next.key) end
local function keyPrev() return getInput(-1, motif[main.group].menu.previous.key) end
local function keyDone() return getInput(-1, motif[main.group].menu.done.key) end
local function keyCancel() return esc() or getInput(-1, motif[main.group].menu.cancel.key) end

local function sndMove() sndPlay(motif.Snd, motif[main.group].cursor.move.snd[1], motif[main.group].cursor.move.snd[2]) end
local function sndDone() sndPlay(motif.Snd, motif[main.group].cursor.done.snd.default[1], motif[main.group].cursor.done.snd.default[2]) end
local function sndCancel() sndPlay(motif.Snd, motif[main.group].cancel.snd[1], motif[main.group].cancel.snd[2]) end

local function message(text)
	main.f_warning(text, motif[main.group], motif[main.background])
end

local function statusLine()
	local st = P.status
	if not P.alive() then
		return P.launchError or 'Starting Ikemen Party...', RED
	end
	if st.lobby_error ~= nil and st.lobby_error ~= '' then
		return st.lobby_error, RED
	end
	if not st.lobby_ok then
		return 'Connecting to the lobby...', GREY
	end
	return string.format('%d session(s) online   |   your version: %s', st.session_count or 0,
		(st.fingerprint ~= '' and st.fingerprint) or '...'), GREEN
end

local function sessionLabel(s)
	local tag = s.mode == 'stream' and '[STREAM] ' or '[NETPLAY]'
	local c = s.counts or {}
	local right
	if s.mode == 'stream' then
		right = string.format('W%d P%d/%d', c.watching or 0, c.playing or 0, c.player_capacity or 0)
	else
		right = (s.status == 'in_match') and 'IN MATCH' or 'OPEN 1/2'
	end
	return tag .. ' ' .. clip(s.name, 28), right
end

local function sessionColor(s)
	if s.mine then return GOLD end
	if s.locked or s.status == 'full' or s.status == 'in_match' then return GREY end
	if s.mode == 'netplay' and not s.match then return RED end
	return WHITE
end

local function sessionDetail(s)
	local parts = {'host ' .. clip(s.host_name, 16), clip(s.game, 18)}
	if s.mode == 'netplay' then
		table.insert(parts, s.match and 'version OK' or 'DIFFERENT VERSION')
		table.insert(parts, s.rollback and 'rollback' or 'delay')
	else
		table.insert(parts, 'opens in browser')
	end
	if s.mine then table.insert(parts, 'YOUR SESSION') end
	return table.concat(parts, '  |  ')
end

-- The netplay modes menu shown once two players are connected.
function P.netplayMenu()
	main.f_clearShuffleTables()
	main.f_menuSnap(motif[main.group])
	main.f_menuItemBgAnimReset(motif[main.group])
	fadeInInit(motif[main.group].fadein.FadeData)
	if main.menu.submenu.server ~= nil and main.menu.submenu.server.loop ~= nil then
		return main.menu.submenu.server.loop() -- the screenpack defines one
	end
	if P.serverMenu == nil then
		local m = {title = 'ONLINE', submenu = {}, items = {
			{itemname = 'netplayversus', displayname = 'VERSUS', paramname = 'server_netplayversus'},
			{itemname = 'netplayteamcoop', displayname = 'ARCADE CO-OP', paramname = 'server_netplayteamcoop'},
			{itemname = 'netplaysurvivalcoop', displayname = 'SURVIVAL CO-OP', paramname = 'server_netplaysurvivalcoop'},
			{itemname = 'back', displayname = 'BACK', paramname = 'server_back'},
		}}
		m.loop = main.f_createMenu(m, false, false, true, false)
		P.serverMenu = m
	end
	P.serverMenu.loop()
end

local function afterNetplay()
	replayStop()
	exitNetPlay()
	exitReplay()
	local w = getSessionWarning()
	if w ~= nil and w ~= '' then
		message(w)
	end
end

-------------------------------------------------------------------------------
-- Actions
-------------------------------------------------------------------------------

function P.joinSession(s)
	if s.mine then
		message('This is your own session.')
		return
	end
	if s.locked then
		message('This session is closed to new people.')
		return
	end
	if s.mode == 'netplay' then
		if not s.match then
			message('Different version.\n\nThe host has different characters, stages or engine files.\n'
				.. 'Netplay needs identical installs, or the match falls out of sync.')
			return
		end
		if s.rollback ~= gameOption('Netplay.RollbackNetcode') then
			message('The host uses ' .. (s.rollback and 'rollback' or 'delay') .. ' netcode.\n\n'
				.. 'Change it in Options > Netplay Settings to match, then try again.')
			return
		end
		if s.status == 'in_match' then
			message('That match is already in progress.')
			return
		end
	end

	local ok, err = P.sendAndWait('join', {session_id = s.id}, 'Joining ' .. clip(s.name, 24), 12,
		function(st) return st.join ~= nil and st.join.done == true and st.join.cmd_id == P.pending.id end)
	if not ok then
		if err ~= 'cancelled' then message(err) end
		return
	end
	local j = P.status.join
	if not j.ok then
		message('Could not join: ' .. tostring(j.error or 'unknown error'))
		return
	end
	if j.mode == 'stream' then
		message('The session opened in your web browser.\n\n'
			.. 'Plug in your controller there and press "Ask to play".')
		return
	end

	-- Netplay: connect with Ikemen's own netcode, on the host's port.
	local oldPort = gameOption('Netplay.ListenPort')
	modifyGameOption('Netplay.ListenPort', j.netplay_port)
	main.f_waitForPreloads(true)
	sndDone()
	if main.f_connect(j.address, s.name) then
		if synchronize() then
			P.netplayMenu()
		end
		afterNetplay()
	end
	modifyGameOption('Netplay.ListenPort', oldPort)
end

function P.hostNetplay()
	local port = gameOption('Netplay.ListenPort')
	local rbPort = gameOption('Netplay.Rollback.Port') or 0
	local rollback = gameOption('Netplay.RollbackNetcode') == true
	if not P.netplayInfoShown then
		P.netplayInfoShown = true
		local ports = 'TCP ' .. port
		if rollback then ports = ports .. ' and UDP ' .. rbPort end
		message('Hosting netplay\n\nPlayers connect straight to your PC.\n'
			.. 'Your router must forward ' .. ports .. ' to this PC.\n'
			.. 'Your public IP is shared with players who join.')
	end
	local ok, err = P.sendAndWait('host_netplay', {
		name = '', port = port, rollback_port = rbPort, rollback = rollback,
	}, 'Listing your session', 12, function(st)
		local h = st.hosting or {}
		return h.mode == 'netplay' and h.state ~= nil and h.state ~= 'off'
	end)
	if not ok then
		if err ~= 'cancelled' then message(err) end
		P.send('stop_hosting')
		return
	end
	local state = P.status.hosting.state or ''
	if state:sub(1, 6) == 'error:' then
		message('Could not list your session:\n' .. state:sub(8))
		P.send('stop_hosting')
		return
	end
	main.f_waitForPreloads(true)
	if main.f_connect('', port) then
		P.send('netplay_status', {status = 'in_match'})
		if synchronize() then
			P.netplayMenu()
		end
		afterNetplay()
	end
	P.send('stop_hosting')
end

-- Stream hosting: the browser shares the game, this screen shows progress.
function P.hostStream()
	P.send('host_stream', {name = ''})
	local items = {'BACK TO MAIN MENU (keep hosting)', 'OPEN HOST PAGE', 'STOP HOSTING'}
	local item = 1
	local ready = false -- ignore the press that opened this screen
	resetKey()
	while true do
		if frame % 10 == 0 then P.tick() end
		if not ready then
			-- first frame: draw only
		elseif keyNext() then item = item % #items + 1; sndMove()
		elseif keyPrev() then item = (item - 2) % #items + 1; sndMove()
		elseif keyCancel() then sndCancel(); return
		elseif keyDone() then
			sndDone()
			if item == 1 then return
			elseif item == 2 then P.send('open_host_page')
			else P.send('stop_hosting'); return end
		end

		beginFrame()
		header('Hosting a stream session')
		local st = P.status or {}
		local h = st.hosting or {}
		local t = st.tunnel or {}
		local y = 56
		local function line(s, c) txtc(clip(s, 52), 24, y, c or WHITE); y = y + 12 end
		line('1. In your browser, click "Share Ikemen window" and pick Ikemen.', WHITE)
		line('2. Come back here and play any mode. Friends join from the lobby.', WHITE)
		y = y + 6
		local state = h.state or 'starting'
		if not P.alive() then state = 'starting Ikemen Party...' end
		local sc = (state == 'listed' and GREEN) or ((state:sub(1, 6) == 'error:') and RED) or GOLD
		line('Lobby:   ' .. (state == 'listed' and 'LISTED - everyone can see you' or state), sc)
		line('Sharing: ' .. (h.sharing and 'yes' or 'not yet'), h.sharing and GREEN or GOLD)
		if t.state == 'downloading' then
			line('Link:    downloading cloudflared (one time)...', GOLD)
		elseif h.join_url ~= nil and h.join_url ~= '' then
			line('Link:    ' .. h.join_url, CYAN)
		else
			line('Link:    ' .. (t.state or '...'), GREY)
		end
		line(string.format('People:  %d watching, %d playing', h.watching or 0, h.playing or 0), WHITE)
		y = 168
		for i, label in ipairs(items) do
			if i == item then box(20, y - 9, W - 20, y + 3, 76, 195, 255, 70) end
			txtc(label, W / 2, y, i == item and CYAN or WHITE, 0)
			y = y + 14
		end
		endFrame()
		ready = true
	end
end

-------------------------------------------------------------------------------
-- Main lobby screen
-------------------------------------------------------------------------------

function P.lobbyLoop()
	local item, top = 1, 1
	local VISIBLE = 9
	P.readStatus()
	if not P.alive() then P.launch() end
	resetKey()
	fadeInInit(motif[main.group].fadein.FadeData)
	local ready = false -- ignore the press that opened this screen
	while true do
		if frame % 10 == 0 then P.tick() end

		-- build the list
		local list = {
			{kind = 'host_stream', label = 'HOST A STREAM', right = 'anyone, up to 8P'},
			{kind = 'host_netplay', label = 'HOST NETPLAY', right = '1v1, same version'},
		}
		local st = P.status
		if P.alive() and st.sessions ~= nil then
			for _, s in ipairs(st.sessions) do
				local l, r = sessionLabel(s)
				table.insert(list, {kind = 'session', s = s, label = l, right = r})
			end
		end
		table.insert(list, {kind = 'back', label = 'BACK'})
		if item > #list then item = #list end

		-- input
		if not ready then
			ready = true -- first frame: draw only
		elseif keyNext() then
			item = item % #list + 1
			sndMove()
		elseif keyPrev() then
			item = (item - 2) % #list + 1
			sndMove()
		elseif keyCancel() then
			sndCancel()
			break
		elseif keyDone() then
			local it = list[item]
			if it.kind == 'back' then
				sndCancel()
				break
			elseif not P.alive() then
				message(P.launchError or 'Ikemen Party is still starting. Try again in a moment.')
			elseif it.kind == 'host_stream' then
				sndDone()
				P.hostStream()
			elseif it.kind == 'host_netplay' then
				sndDone()
				P.hostNetplay()
			elseif it.kind == 'session' then
				sndDone()
				P.joinSession(it.s)
			end
			resetKey()
			ready = false -- the next screen's last press must not count here
		end
		if item < top then top = item end
		if item >= top + VISIBLE then top = item - VISIBLE + 1 end

		-- draw
		beginFrame()
		local sline, scol = statusLine()
		header()
		txtc(clip(sline, 52), W / 2, 32, scol, 0)
		local y = 52
		for i = top, math.min(#list, top + VISIBLE - 1) do
			local it = list[i]
			local c = WHITE
			if it.kind == 'session' then c = sessionColor(it.s)
			elseif it.kind ~= 'back' then c = GOLD end
			if i == item then
				box(12, y - 9, W - 12, y + 3, 76, 195, 255, 70)
			end
			txtc(it.label, 18, y, i == item and CYAN or c)
			if it.right ~= nil then txtc(it.right, W - 18, y, GREY, -1) end
			if i == 2 and #list > 3 then
				box(16, y + 6, W - 16, y + 7, 38, 49, 74, 255)
				y = y + 4
			end
			y = y + 14
		end
		if P.alive() and st.sessions ~= nil and #st.sessions == 0 and (st.lobby_error or '') == '' then
			txtc('Nobody is hosting right now. Be the first!', W / 2, y + 6, GREY, 0)
		end
		local sel = list[item]
		if sel.kind == 'session' then
			txtc(clip(sessionDetail(sel.s), 52), W / 2, 206, GREY, 0)
		end
		txtc('Confirm: select   Cancel: back', W / 2, 226, GREY, 0)
		endFrame()
	end
	resetKey()
	return nil
end

-------------------------------------------------------------------------------
-- Register the menu item (before OPTIONS on the title screen)
-------------------------------------------------------------------------------

main.t_itemname.onlinelobby = function(t, item)
	return P.lobbyLoop
end
if motif[main.group] ~= nil and motif[main.group].menu ~= nil then
	main.f_appendItemname(motif[main.group].menu, '', 'onlinelobby', 'ONLINE LOBBY', 'options')
end

ikemenParty = P -- for other mods / debugging
print('Ikemen Party Online Lobby mod ' .. P.VERSION .. ' loaded')
