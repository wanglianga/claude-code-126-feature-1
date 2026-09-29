-- 种子数据：2 门店、各角色账号、6 类商品、配方、库存

INSERT INTO stores (code,name,region,capacity_per_5m,max_active_cups,stale_minutes) VALUES
 ('SH001','上海人民广场店','华东',6,10,2),
 ('SH002','上海陆家嘴店','华东',8,12,2)
ON CONFLICT (code) DO NOTHING;

INSERT INTO accounts (login,login_code,name,role,store_id,region,platform) VALUES
 ('admin','0000','平台管理员','admin',NULL,'华东',NULL),
 ('mgr.huadong','1001','王经理','area_manager',NULL,'华东',NULL),
 ('sup.li','1002','李督导','supervisor',NULL,'华东',NULL),
 ('sm.001','2001','赵店长','store_manager',1,NULL,NULL),
 ('barista.001','3001','小钱','barista',1,NULL,NULL),
 ('barista.002','3002','小孙','barista',1,NULL,NULL),
 ('rider.eleme','4001','老周','rider',NULL,NULL,'eleme'),
 ('rider.meituan','4002','老吴','rider',NULL,NULL,'meituan'),
 ('cs.eleme','5001','饿了么客服小何','platform_cs',NULL,NULL,'eleme'),
 ('cs.meituan','5002','美团客服小冯','platform_cs',NULL,NULL,'meituan'),
 ('finance','6001','财务小郑','finance',NULL,NULL,NULL),
 ('customer.001','7001','顾客小陈','customer',NULL,NULL,'eleme'),
 ('platform.eleme','9001','饿了么平台','platform',NULL,NULL,'eleme'),
 ('platform.meituan','9002','美团平台','platform',NULL,NULL,'meituan')
ON CONFLICT (login) DO NOTHING;

INSERT INTO menu_items (sku,name,category,is_drink,includes_drink,price,make_seconds) VALUES
 ('AM','美式','americano',TRUE,FALSE,15.00,90),
 ('LT','拿铁','latte',TRUE,FALSE,22.00,150),
 ('CB','冷萃','cold_brew',TRUE,FALSE,25.00,60),
 ('FC','果咖','fruit_coffee',TRUE,FALSE,24.00,180),
 ('LF','鸡肉轻食卷','light_food',FALSE,FALSE,28.00,120),
 ('SET','元气套餐(美式+轻食)','combo',FALSE,TRUE,38.00,200)
ON CONFLICT (sku) DO NOTHING;

INSERT INTO ingredients (sku,name,unit,cost_per_unit) VALUES
 ('bean','咖啡豆','g',0.05),
 ('milk','牛奶','ml',0.012),
 ('coldbase','冷萃液','ml',0.03),
 ('fruit','水果(橙/莓)','g',0.08),
 ('cupM','M杯+盖+杯贴','套',0.35),
 ('cupL','L杯+盖+杯贴','套',0.45),
 ('cupXL','XL杯+盖+杯贴','套',0.55),
 ('ice','冰块','g',0.002),
 ('sugar','糖浆','份',0.10),
 ('straw','吸管','支',0.05),
 ('utensil','餐具包','份',0.30),
 ('seal','封签','张',0.02),
 ('wrap','轻食包装','个',0.60),
 ('bagcold','冷袋','个',0.40),
 ('baghot','热袋','个',0.35),
 ('bagfood','餐食袋','个',0.35)
ON CONFLICT (sku) DO NOTHING;

-- 配方：杯耗材按 applicable_sizes 匹配杯型（NULL=任意杯型）；冰饮额外加冰（qty_if_ice）；所有饮品需要封签
INSERT INTO recipes (menu_item_id,ingredient_id,qty_per_cup,qty_if_ice,applicable_sizes)
SELECT m.id,i.id,v.q,v.ice,v.sizes FROM (VALUES
 ('AM','bean',18,0,NULL),('AM','cupM',1,0,'M'),('AM','cupL',1,0,'L'),('AM','ice',0,120,NULL),('AM','sugar',1,0,NULL),('AM','seal',1,0,NULL),('AM','straw',0,1,NULL),
 ('LT','bean',18,0,NULL),('LT','milk',180,0,NULL),('LT','cupM',1,0,'M'),('LT','cupL',1,0,'L'),('LT','ice',0,120,NULL),('LT','sugar',1,0,NULL),('LT','seal',1,0,NULL),('LT','straw',0,1,NULL),
 ('CB','coldbase',300,0,NULL),('CB','cupL',1,0,'L'),('CB','cupXL',1,0,'XL'),('CB','ice',0,100,NULL),('CB','seal',1,0,NULL),('CB','straw',0,1,NULL),
 ('FC','bean',15,0,NULL),('FC','fruit',80,0,NULL),('FC','cupL',1,0,'L'),('FC','ice',0,150,NULL),('FC','sugar',1,0,NULL),('FC','seal',1,0,NULL),('FC','straw',0,1,NULL),
 ('LF','wrap',1,0,NULL),('LF','utensil',1,0,NULL),
 ('SET','bean',18,0,NULL),('SET','cupM',1,0,'M'),('SET','cupL',1,0,'L'),('SET','ice',0,120,NULL),('SET','sugar',1,0,NULL),('SET','seal',1,0,NULL),('SET','straw',0,1,NULL),('SET','wrap',1,0,NULL),('SET','utensil',1,0,NULL)
) AS v(menu,ing,q,ice,sizes)
JOIN menu_items m ON m.sku=v.menu JOIN ingredients i ON i.sku=v.ing
ON CONFLICT (menu_item_id,ingredient_id) DO NOTHING;

-- 库存（两门店一致），冷萃液故意给少一点以便演示缺货
INSERT INTO inventory (store_id,ingredient_id,qty)
SELECT s.id,i.id,v.q FROM stores s CROSS JOIN ingredients i
JOIN (VALUES
 ('bean',2000),('milk',20000),('coldbase',5000),('fruit',3000),
 ('cupM',300),('cupL',300),('cupXL',100),('ice',20000),('sugar',500),
 ('straw',400),('utensil',200),('seal',800),('wrap',150),
 ('bagcold',200),('baghot',200),('bagfood',200)
) AS v(sku,q) ON v.sku=i.sku
ON CONFLICT (store_id,ingredient_id) DO NOTHING;
